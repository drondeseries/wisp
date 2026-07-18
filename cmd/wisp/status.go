package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/store"
)

// Request states the status API reports, designed to map onto Silo's
// request_router statuses.
const (
	statusQueued    = "queued"    // tracked, nothing (in scope) pinned yet — includes unreleased/unaired
	statusCompleted = "completed" // requested scope pinned and servable
	statusFailed    = "failed"    // permanent give-up (unresolvable identity)
)

// requestStatus is wisp's authoritative view of a title, computed purely from
// the monitor record and the pin store — no network calls.
type requestStatus struct {
	State           string   `json:"state"`
	PinnedQualities []string `json:"pinned_qualities"`
	Detail          string   `json:"detail"`
	RequestRef      string   `json:"request_ref,omitempty"`
}

// handleRequestStatus reports a title's state — GET /api/requests/status. The
// title is identified by media_type + tmdb_id, falling back to imdb_id. A 404
// means wisp is not tracking the title (no monitor and no pins).
func (a *app) handleRequestStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mediaType := strings.TrimSpace(q.Get("media_type"))
	tmdbID := strings.TrimSpace(q.Get("tmdb_id"))
	imdbID := strings.TrimSpace(q.Get("imdb_id"))
	if tmdbID == "" && imdbID == "" {
		http.Error(w, "provide tmdb_id or imdb_id", http.StatusBadRequest)
		return
	}

	mon := a.findMonitor(r.Context(), mediaType, tmdbID, imdbID)
	var searchIDs []string
	if mon != nil {
		searchIDs = append(searchIDs, monitorSearchID(*mon))
		if mon.TMDbID != "" && tmdbID == "" {
			tmdbID = mon.TMDbID // let the monitor's tmdb id find legacy imdb-keyed pins
		}
	} else {
		if imdbID != "" {
			searchIDs = append(searchIDs, imdbID)
		}
		if tmdbID != "" {
			searchIDs = append(searchIDs, "tmdb:"+tmdbID)
		}
	}
	pins := a.servablePins(r.Context(), searchIDs, tmdbID)

	if mon == nil && len(pins) == 0 {
		http.NotFound(w, r) // not tracked — the caller should (re)submit via /api/add
		return
	}
	writeJSON(w, computeRequestStatus(mon, pins, mediaType, time.Now()))
}

// findMonitor locates the monitored record for a title by tmdb id (preferred)
// or imdb id, honoring media_type when supplied. Monitors are few, so a scan is
// cheaper than maintaining a secondary index.
func (a *app) findMonitor(ctx context.Context, mediaType, tmdbID, imdbID string) *store.Monitored {
	items, err := a.store.ListMonitored(ctx)
	if err != nil {
		return nil
	}
	for i := range items {
		it := items[i]
		if mediaType != "" && it.MediaType != mediaType {
			continue
		}
		if (tmdbID != "" && it.TMDbID == tmdbID) || (imdbID != "" && it.IMDbID == imdbID) {
			return &it
		}
	}
	return nil
}

// servablePins returns the healthy (resolvable) pins for a title, looked up both
// by search id (imdb or "tmdb:<id>" slot) and by the persisted bare TMDbID, so
// legacy/direct pins keyed by imdb are found on a tmdb-only query. Deduped by
// virtual path.
func (a *app) servablePins(ctx context.Context, searchIDs []string, tmdbID string) []store.Pin {
	var out []store.Pin
	seen := map[string]bool{}
	add := func(pins []store.Pin) {
		for _, p := range pins {
			if p.Servable() && !seen[p.VirtualPath] {
				seen[p.VirtualPath] = true
				out = append(out, p)
			}
		}
	}
	for _, id := range searchIDs {
		if pins, err := a.store.PinsByMedia(ctx, id); err == nil {
			add(pins)
		}
	}
	if pins, err := a.store.PinsByTMDbID(ctx, tmdbID); err == nil {
		add(pins)
	}
	return out
}

// computeRequestStatus maps a monitor + its servable pins onto the request
// state. mediaType is the query hint, used when no monitor/pin fixes it.
//
// Mapping rules (from the architecture memo):
//   - failed: permanent give-up only (Monitored.Failed) — never for an
//     unreleased/unaired title.
//   - completed: requested scope pinned and servable. Movie: a servable pin
//     exists. Series: a servable pin exists AND the scheduler's last pass found
//     no aired-but-unpinned episodes (PendingAired == 0). Series monitors keep
//     running for future episodes but still report completed.
//   - queued: otherwise — tracked but nothing in scope pinned yet, whether
//     unreleased/unaired or in the released-but-no-stream-yet retry window.
func computeRequestStatus(mon *store.Monitored, pins []store.Pin, mediaType string, now time.Time) requestStatus {
	// Only servable pins count toward completion or pinned_qualities — a pin whose
	// stream is gone is not "done". (The HTTP path pre-filters; this keeps the
	// function correct for any caller.)
	servable := pins[:0:0]
	for _, p := range pins {
		if p.Servable() {
			servable = append(servable, p)
		}
	}
	pins = servable

	mt := mediaType
	if mt == "" && mon != nil {
		mt = mon.MediaType
	}
	if mt == "" && len(pins) > 0 {
		mt = pins[0].MediaType
	}

	st := requestStatus{PinnedQualities: pinnedQualities(pins)}
	if mon != nil {
		st.RequestRef = mon.RequestRef
	}

	switch {
	case mon != nil && mon.Failed:
		st.State = statusFailed
		st.Detail = failDetail(mon)
	case isCompleted(mt, mon, pins):
		st.State = statusCompleted
		st.Detail = "requested scope pinned"
	default:
		st.State = statusQueued
		st.Detail = queuedDetail(mon, now)
	}
	return st
}

// isCompleted reports whether the requested scope is pinned and servable.
func isCompleted(mediaType string, mon *store.Monitored, pins []store.Pin) bool {
	if len(pins) == 0 {
		return false
	}
	if mediaType == "series" {
		// Need the scheduler's aired-coverage signal; without a monitor we can't
		// confirm every aired episode is present.
		return mon != nil && !mon.LastChecked.IsZero() && mon.PendingAired == 0
	}
	// Movie: every requested quality tier must have a servable pin — otherwise a
	// 1080p pin would report "completed" while a later 2160p request is still
	// unfulfilled, and the router would stop polling. A legacy direct pin (no
	// monitor) has no requested-tier list, so any servable pin is the whole scope.
	if mon == nil {
		return true
	}
	return allTiersPinned(mon.Qualities, pins)
}

// allTiersPinned reports whether every requested quality tier has a servable
// pin. An empty request list means "best available" — satisfied by any pin.
func allTiersPinned(requested []string, pins []store.Pin) bool {
	present := make(map[string]bool, len(pins))
	for _, p := range pins {
		present[library.NormalizeQuality(p.Quality)] = true
	}
	want := make([]string, 0, len(requested))
	for _, q := range requested {
		if n := library.NormalizeQuality(q); n != "" {
			want = append(want, n)
		}
	}
	if len(want) == 0 {
		return len(pins) > 0
	}
	for _, q := range want {
		if !present[q] {
			return false
		}
	}
	return true
}

// pinnedQualities returns the sorted, unique quality tiers among the pins.
func pinnedQualities(pins []store.Pin) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pins {
		q := library.NormalizeQuality(p.Quality)
		if q == "" {
			q = p.Quality
		}
		if q != "" && !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	sort.Strings(out)
	return out
}

func failDetail(mon *store.Monitored) string {
	if mon.LastError != "" {
		return mon.LastError
	}
	return "permanent failure: unresolvable identity"
}

// queuedDetail explains why a queued title is not yet completed, using only
// stored state (DueReason/DueAt), so a caller can distinguish "awaiting release"
// from "resolving stream".
func queuedDetail(mon *store.Monitored, now time.Time) string {
	if mon == nil {
		return "resolving stream"
	}
	if mon.DueAt.After(now) {
		switch mon.DueReason {
		case store.DueReasonRelease:
			return "awaiting home-media release"
		case store.DueReasonAirstamp:
			return "awaiting next episode airing"
		}
	}
	return "resolving stream"
}

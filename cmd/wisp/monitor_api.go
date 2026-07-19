package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/monitor"
)

// Pin implements monitor.Fulfiller: resolve+pin one target. pinned=false means
// no stream is available yet (the monitor keeps trying); a returned error is a
// real fault (auth/rate-limit/store) worth surfacing.
func (a *app) Pin(ctx context.Context, t monitor.Target) (bool, error) {
	_, _, err := a.pin(ctx, pinSpec{
		MediaType: t.MediaType, IMDbID: t.IMDbID, TMDbID: t.TMDbID, TVDbID: t.TVDbID,
		Title: t.Title, Year: t.Year, Season: t.Season, Episode: t.Episode, Quality: t.Quality,
		Category: t.Category,
	})
	if err == nil {
		return true, nil
	}
	if reason := noStreamReason(err); reason != "" {
		// Surface WHY nothing pinned, at INFO — otherwise a title that AIOStreams
		// can't resolve (e.g. a debrid returning no playable links) just sits in
		// "retrying" with no explanation, which looks like wisp doing nothing.
		a.log.Info("no stream to pin yet", "reason", reason, "title", t.Title,
			"media_type", t.MediaType, "imdb", t.IMDbID, "season", t.Season,
			"episode", t.Episode, "quality", t.Quality)
		return false, nil // nothing (at this quality) to pin yet
	}
	return false, err
}

// noStreamReason classifies an "unable to pin yet" error for logging. It returns
// "" when err is a genuine fault (auth/rate-limit/store) that should propagate as
// an error instead of a benign retry.
func noStreamReason(err error) string {
	switch {
	case errors.Is(err, errNoResults):
		return "AIOStreams returned no playable results (check debrid/indexer)"
	case errors.Is(err, errNoPlayable):
		return "results found but none were probeable"
	case errors.Is(err, errNoQualityMatch):
		return "no result at the requested quality"
	default:
		return ""
	}
}

// PinnedKeys implements monitor.Fulfiller: what's already pinned for an id.
func (a *app) PinnedKeys(ctx context.Context, imdbID string) (map[monitor.PinKey]bool, error) {
	pins, err := a.store.PinsByMedia(ctx, imdbID)
	if err != nil {
		return nil, err
	}
	keys := make(map[monitor.PinKey]bool, len(pins))
	for _, p := range pins {
		keys[monitor.PinKey{Season: p.Season, Episode: p.Episode, Quality: library.NormalizeQuality(p.Quality)}] = true
	}
	return keys, nil
}

type monitorRequest struct {
	MediaType string   `json:"media_type"`
	IMDbID    string   `json:"imdb_id"`
	TMDbID    string   `json:"tmdb_id"`
	TVDbID    string   `json:"tvdb_id"`
	Title     string   `json:"title"`
	Year      int      `json:"year"`
	Qualities []string `json:"qualities"`
	Seasons   []int    `json:"seasons"`
}

// handleCreateMonitor registers a monitor directly (media-server-neutral, no
// request tool required) — POST /api/monitors.
func (a *app) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.MediaType != "movie" && req.MediaType != "series" {
		http.Error(w, "media_type must be movie or series", http.StatusBadRequest)
		return
	}
	if req.IMDbID == "" && req.TMDbID == "" {
		http.Error(w, "imdb_id or tmdb_id is required", http.StatusBadRequest)
		return
	}
	if err := a.mon.Intake(r.Context(), monitor.Request{
		MediaType: req.MediaType, IMDbID: req.IMDbID, TMDbID: req.TMDbID, TVDbID: req.TVDbID,
		Title: req.Title, Year: req.Year, Qualities: normalizeQualities(req.Qualities), Seasons: req.Seasons,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"monitoring": true})
}

// handleListMonitors returns the watchlist — GET /api/monitors.
func (a *app) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListMonitored(r.Context())
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, items)
}

// handleDeleteMonitor stops monitoring a title — DELETE /api/monitors?key=…
func (a *app) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "provide ?key=", http.StatusBadRequest)
		return
	}
	if err := a.store.DeleteMonitored(r.Context(), key); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": key})
}

// handleRefreshMonitors triggers an immediate scheduler pass — POST /api/monitors/refresh.
func (a *app) handleRefreshMonitors(w http.ResponseWriter, r *http.Request) {
	a.mon.Wake()
	writeJSON(w, map[string]any{"refreshing": true})
}

func normalizeQualities(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range in {
		if n := library.NormalizeQuality(q); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

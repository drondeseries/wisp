// Package monitor keeps a persistent watchlist of titles wisp can't pin yet —
// unreleased movies and ongoing series — and pins them as they become available.
//
// It is push-first everywhere it can be: intake is webhook-driven (see the seerr
// package) and the media server is notified by webhook. The one thing nothing
// pushes is "a new episode aired / is now seedable", so the scheduler handles
// that — but it wakes near a known next airstamp rather than polling blindly,
// with the configured interval only as a fallback ceiling.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

// PinKey identifies a pinned unit for dedupe. Quality is canonical ("" only for
// a pin whose resolution was unknown).
type PinKey struct {
	Season  int
	Episode int
	Quality string
}

// Target is one concrete thing to resolve and pin: a movie (season/episode 0)
// or an episode, at a specific quality ("" = best available).
type Target struct {
	MediaType string
	IMDbID    string
	TMDbID    string
	TVDbID    string
	Title     string
	Year      int
	Season    int
	Episode   int
	Quality   string
}

// Request is an intake from a feeder (Seerr): a movie or a whole series, at zero
// or more requested quality tiers.
type Request struct {
	MediaType string
	IMDbID    string
	TMDbID    string
	TVDbID    string
	Title     string
	Year      int
	Qualities []string
	Seasons   []int // series: requested seasons; empty = all
}

// Fulfiller resolves+pins targets and reports what is already pinned. The app
// implements it over AIOStreams + the pin store.
type Fulfiller interface {
	// Pin resolves and pins one target; pinned=false means "no stream yet".
	Pin(ctx context.Context, t Target) (pinned bool, err error)
	// PinnedKeys returns the units already pinned for an IMDb id (for dedupe).
	PinnedKeys(ctx context.Context, imdbID string) (map[PinKey]bool, error)
}

// Monitor tracks and schedules pending titles.
type Monitor struct {
	store    *store.Store
	meta     *metadata.Service
	ful      Fulfiller
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time
	wake     chan struct{}
	// nextWakeNano is the unix-nano deadline of the sleep timer the Run loop last
	// armed, published so the schedule API reports the scheduler's real next wake
	// rather than a reconstruction. Zero until the first pass arms a timer.
	nextWakeNano atomic.Int64
	// mu serializes read-modify-write of a monitored item so the scheduler and a
	// concurrent Intake/delete can't clobber each other (network processing runs
	// outside the lock).
	mu sync.Mutex
}

// New builds a monitor. interval is the fallback re-check ceiling.
func New(st *store.Store, meta *metadata.Service, ful Fulfiller, interval time.Duration, log *slog.Logger) *Monitor {
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	return &Monitor{
		store: st, meta: meta, ful: ful, log: log, interval: interval,
		now:  time.Now,
		wake: make(chan struct{}, 1),
	}
}

// Interval reports the fallback re-check ceiling — the longest the scheduler
// will sleep when no sooner airstamp/release is known.
func (m *Monitor) Interval() time.Duration { return m.interval }

// NextWake reports when the scheduler's sleep timer is next set to fire. It
// returns the zero time before the first pass has armed a timer.
func (m *Monitor) NextWake() time.Time {
	nano := m.nextWakeNano.Load()
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// Intake registers a request and wakes the scheduler to act on it immediately.
// A tmdb-only request is resolved to its imdb id (series enumeration needs it).
func (m *Monitor) Intake(ctx context.Context, r Request) error {
	if r.IMDbID == "" && r.TMDbID != "" && m.meta.HasTMDB() {
		if imdb, err := m.meta.IMDbForTMDB(ctx, r.MediaType, r.TMDbID); err == nil {
			r.IMDbID = imdb
		}
	}
	if r.MediaType == "series" && r.IMDbID == "" {
		return fmt.Errorf("series intake requires an imdb id (tmdb→imdb lookup failed)")
	}
	if r.IMDbID == "" && r.TMDbID == "" {
		return fmt.Errorf("intake requires an imdb or tmdb id")
	}
	// A movie is only gatable if a release date can be sourced: via Cinemeta
	// (needs imdb) or TMDB (needs tmdb + a key). Otherwise it would retry forever.
	if r.MediaType == "movie" && r.IMDbID == "" && (r.TMDbID == "" || !m.meta.HasTMDB()) {
		return fmt.Errorf("movie intake needs an imdb id or a tmdb id with WISP_TMDB_API_KEY set (no way to gate release otherwise)")
	}
	item := store.Monitored{
		Key: monitorKey(r.MediaType, r.IMDbID, r.TMDbID), MediaType: r.MediaType,
		IMDbID: r.IMDbID, TMDbID: r.TMDbID, TVDbID: r.TVDbID, Title: r.Title,
		Year: r.Year, Qualities: r.Qualities, Seasons: r.Seasons, DueAt: m.now(),
		Enabled: true,
	}
	m.mu.Lock()
	// A later request for the same title extends the existing monitor (e.g. a new
	// season, or a 4K request on top of HD) rather than replacing it. A request
	// that scopes no seasons widens coverage to all seasons. Re-requesting resets
	// DueAt to now and clears Completed so the new work is picked up.
	if existing, _ := m.store.GetMonitored(ctx, item.Key); existing != nil {
		item.AddedAt = existing.AddedAt
		item.Qualities = unionStrings(existing.Qualities, r.Qualities)
		if len(existing.Seasons) == 0 || len(r.Seasons) == 0 {
			item.Seasons = nil // one unscoped request means "all seasons"
		} else {
			item.Seasons = unionInts(existing.Seasons, r.Seasons)
		}
	}
	err := m.store.PutMonitored(ctx, item)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.log.Info("monitoring", "key", item.Key, "title", item.Title)
	m.Wake()
	return nil
}

// Run drives the scheduler until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	for {
		next := m.checkDue(ctx)
		delay := m.interval
		if !next.IsZero() {
			if d := next.Sub(m.now()); d < delay {
				delay = d
			}
		}
		if delay < time.Second {
			delay = time.Second // never busy-loop
		}
		m.nextWakeNano.Store(m.now().Add(delay).UnixNano())
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-m.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Wake nudges the scheduler to run a pass now (e.g. after Intake).
func (m *Monitor) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// checkDue processes every due item and returns the earliest next-due time
// across all remaining items (zero if none).
func (m *Monitor) checkDue(ctx context.Context) time.Time {
	items, err := m.store.ListMonitored(ctx)
	if err != nil {
		m.log.Error("list monitored", "error", err)
		return time.Time{}
	}
	now := m.now()
	var earliest time.Time
	for _, it := range items {
		if !it.Enabled || it.Completed {
			continue // paused, or a fully-pinned movie kept for history
		}
		if it.DueAt.After(now) {
			if earliest.IsZero() || it.DueAt.Before(earliest) {
				earliest = it.DueAt
			}
			continue // not due yet
		}
		// Process outside the lock (it's network-bound); persistResult then folds
		// the result into the current record without clobbering a concurrent
		// Intake or delete.
		var (
			due       time.Time
			completed bool
			reason    string
			errMsg    string
		)
		if it.MediaType == "series" {
			due, reason, errMsg = m.processSeries(ctx, it)
		} else {
			due, completed, reason, errMsg = m.processMovie(ctx, it)
		}
		if effective := m.persistResult(ctx, it, due, completed, reason, errMsg); !effective.IsZero() {
			if earliest.IsZero() || effective.Before(earliest) {
				earliest = effective
			}
		}
	}
	return earliest
}

// persistResult folds a scheduler pass's outcome (next-due, reason, completed,
// error) into the stored item under the lock. If a concurrent Intake changed the
// item since our snapshot (UpdatedAt differs), its DueAt/DueReason/Completed win
// — we only record LastChecked/LastError. Returns the item's effective next-due
// time, or zero if it was deleted or is complete (no wake needed).
func (m *Monitor) persistResult(ctx context.Context, snapshot store.Monitored, due time.Time, completed bool, reason, errMsg string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.store.GetMonitored(ctx, snapshot.Key)
	if err != nil || cur == nil {
		return time.Time{} // deleted concurrently — drop
	}
	cur.LastChecked = m.now()
	cur.LastError = errMsg
	if cur.UpdatedAt.Equal(snapshot.UpdatedAt) {
		cur.DueAt = due
		cur.DueReason = reason
		if snapshot.MediaType != "series" {
			cur.Completed = completed
		}
	}
	if e := m.store.PutMonitored(ctx, *cur); e != nil {
		m.log.Warn("update monitored", "key", cur.Key, "error", e)
	}
	if cur.Completed {
		return time.Time{}
	}
	return cur.DueAt
}

// processMovie pins a released+available movie (marking it Completed) or returns
// when to look again. completed is true once every requested quality is pinned;
// the item is kept (not deleted) so the monitor list is a request history.
func (m *Monitor) processMovie(ctx context.Context, it store.Monitored) (next time.Time, completed bool, reason, errMsg string) {
	now := m.now()
	release, err := m.meta.MovieReleaseDate(ctx, it.IMDbID, it.TMDbID, now)
	switch {
	case errors.Is(err, metadata.ErrNoHomeRelease):
		return now.Add(m.interval), false, store.DueReasonRetry, "" // theatrical-only — check again later
	case err != nil:
		m.log.Warn("movie release lookup", "title", it.Title, "error", err)
		return now.Add(m.interval), false, store.DueReasonRetry, err.Error()
	case release.After(now):
		return release, false, store.DueReasonRelease, "" // wake at the real release date
	}
	pinned, err := m.ful.PinnedKeys(ctx, monitoredSearchID(it))
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	if m.pinMissing(ctx, targetsForQualities(it, 0, 0), pinned) == 0 {
		return time.Time{}, true, store.DueReasonRetry, "" // fully pinned — done, kept for history
	}
	return now.Add(m.interval), false, store.DueReasonRetry, "" // released but no stream yet — retry
}

// processSeries pins any aired-but-unpinned episodes and schedules the next wake
// at the next known airstamp. A series is never completed (it may add seasons).
func (m *Monitor) processSeries(ctx context.Context, it store.Monitored) (next time.Time, reason, errMsg string) {
	now := m.now()
	all, err := m.meta.Episodes(ctx, it.IMDbID)
	if err != nil {
		m.log.Warn("series enumerate", "title", it.Title, "error", err)
		return now.Add(m.interval), store.DueReasonRetry, err.Error()
	}
	if len(it.Seasons) > 0 {
		all = filterSeasons(all, it.Seasons) // honor a per-season Seerr request
	}
	pinned, err := m.ful.PinnedKeys(ctx, monitoredSearchID(it))
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	remaining := 0
	for _, ep := range all {
		if ep.Aired.IsZero() || ep.Aired.After(now) {
			continue // not aired yet
		}
		remaining += m.pinMissing(ctx, targetsForQualities(it, ep.Season, ep.Number), pinned)
	}
	nextAir, hasNext := metadata.NextAir(all, now)
	// A stream usually lags an episode's air time (minutes to hours). If an aired
	// episode is still unpinned, retry at the interval — don't defer to the next
	// airstamp, which could be a week (or a mid-season gap) away.
	if remaining > 0 {
		retry := now.Add(m.interval)
		if hasNext && nextAir.Before(retry) {
			return nextAir, store.DueReasonAirstamp, ""
		}
		return retry, store.DueReasonRetry, ""
	}
	if hasNext {
		return nextAir, store.DueReasonAirstamp, "" // all aired episodes pinned — wake near the next airing
	}
	return now.Add(m.interval), store.DueReasonRetry, "" // no known upcoming episode — check again at the ceiling
}

// pinMissing pins every target not already pinned, returning how many remain
// unpinned (0 = fully satisfied). It updates pinned as it succeeds so multiple
// targets in one pass dedupe against each other.
func (m *Monitor) pinMissing(ctx context.Context, targets []Target, pinned map[PinKey]bool) (remaining int) {
	for _, t := range targets {
		if isPinned(pinned, t.Season, t.Episode, t.Quality) {
			continue
		}
		ok, err := m.ful.Pin(ctx, t)
		if err != nil {
			m.log.Warn("monitor pin", "title", t.Title, "season", t.Season, "episode", t.Episode, "error", err)
			remaining++
			continue
		}
		if !ok {
			remaining++ // no stream yet
			continue
		}
		pinned[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
		m.log.Info("pinned", "title", t.Title, "season", t.Season, "episode", t.Episode, "quality", t.Quality)
	}
	return remaining
}

// isPinned reports whether a unit is already pinned. A specific quality matches
// exactly; a default ("") request is satisfied by any quality of that unit.
func isPinned(pinned map[PinKey]bool, season, episode int, quality string) bool {
	q := library.NormalizeQuality(quality)
	if q == "" {
		for k := range pinned {
			if k.Season == season && k.Episode == episode {
				return true
			}
		}
		return false
	}
	return pinned[PinKey{season, episode, q}]
}

func targetsForQualities(it store.Monitored, season, episode int) []Target {
	quals := it.Qualities
	if len(quals) == 0 {
		quals = []string{""} // default: pin the best available
	}
	out := make([]Target, 0, len(quals))
	for _, q := range quals {
		out = append(out, Target{
			MediaType: it.MediaType, IMDbID: it.IMDbID, TMDbID: it.TMDbID, TVDbID: it.TVDbID,
			Title: it.Title, Year: it.Year, Season: season, Episode: episode, Quality: q,
		})
	}
	return out
}

func filterSeasons(eps []metadata.Episode, seasons []int) []metadata.Episode {
	want := make(map[int]bool, len(seasons))
	for _, s := range seasons {
		want[s] = true
	}
	out := eps[:0:0]
	for _, e := range eps {
		if want[e.Season] {
			out = append(out, e)
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func unionInts(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range append(append([]int{}, a...), b...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// monitoredSearchID is the id an item's pins are stored under — the same
// fallback app.pin uses (imdb, else "tmdb:<id>") — so dedupe lookups match.
func monitoredSearchID(it store.Monitored) string {
	if it.IMDbID != "" {
		return it.IMDbID
	}
	return "tmdb:" + it.TMDbID
}

func monitorKey(mediaType, imdb, tmdb string) string {
	id := imdb
	if id == "" {
		id = "tmdb:" + tmdb
	}
	return mediaType + ":" + id
}

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
	item := store.Monitored{
		Key: monitorKey(r.MediaType, r.IMDbID, r.TMDbID), MediaType: r.MediaType,
		IMDbID: r.IMDbID, TMDbID: r.TMDbID, TVDbID: r.TVDbID, Title: r.Title,
		Year: r.Year, Qualities: r.Qualities, DueAt: m.now(),
	}
	if err := m.store.PutMonitored(ctx, item); err != nil {
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
		due := it.DueAt
		if !due.After(now) {
			var retire bool
			if it.MediaType == "series" {
				due = m.processSeries(ctx, it)
			} else {
				due, retire = m.processMovie(ctx, it)
			}
			if retire {
				if err := m.store.DeleteMonitored(ctx, it.Key); err != nil {
					m.log.Warn("retire monitored", "key", it.Key, "error", err)
				}
				continue
			}
			it.DueAt = due
			if err := m.store.PutMonitored(ctx, it); err != nil {
				m.log.Warn("update monitored", "key", it.Key, "error", err)
			}
		}
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	return earliest
}

// processMovie pins a released+available movie (retiring it) or returns when to
// look again.
func (m *Monitor) processMovie(ctx context.Context, it store.Monitored) (next time.Time, retire bool) {
	now := m.now()
	release, err := m.meta.MovieReleaseDate(ctx, it.IMDbID, it.TMDbID, now)
	switch {
	case errors.Is(err, metadata.ErrNoHomeRelease):
		return now.Add(m.interval), false // theatrical-only — check again later
	case err != nil:
		m.log.Warn("movie release lookup", "title", it.Title, "error", err)
		return now.Add(m.interval), false
	case release.After(now):
		return release, false // wake at release
	}
	pinned, err := m.ful.PinnedKeys(ctx, it.IMDbID)
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	if m.pinMissing(ctx, targetsForQualities(it, 0, 0), pinned) == 0 {
		return time.Time{}, true // fully pinned — done
	}
	return now.Add(m.interval), false // released but no stream yet — retry
}

// processSeries pins any aired-but-unpinned episodes and schedules the next wake
// at the next known airstamp. A series is never retired (it may add seasons).
func (m *Monitor) processSeries(ctx context.Context, it store.Monitored) time.Time {
	now := m.now()
	all, err := m.meta.Episodes(ctx, it.IMDbID)
	if err != nil {
		m.log.Warn("series enumerate", "title", it.Title, "error", err)
		return now.Add(m.interval)
	}
	pinned, err := m.ful.PinnedKeys(ctx, it.IMDbID)
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	for _, ep := range all {
		if ep.Aired.IsZero() || ep.Aired.After(now) {
			continue // not aired yet
		}
		m.pinMissing(ctx, targetsForQualities(it, ep.Season, ep.Number), pinned)
	}
	if next, ok := metadata.NextAir(all, now); ok {
		return next // wake right around the next airing
	}
	return now.Add(m.interval) // no known upcoming episode — check again at the ceiling
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

func monitorKey(mediaType, imdb, tmdb string) string {
	id := imdb
	if id == "" {
		id = "tmdb:" + tmdb
	}
	return mediaType + ":" + id
}

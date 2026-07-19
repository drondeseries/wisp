// Package monitor keeps a persistent watchlist of titles wisp can't pin yet —
// unreleased movies and ongoing series — and pins them as they become available.
//
// It is push-first everywhere it can be: intake is request-driven (Silo's
// request router via the request-shaped /api/add, or POST /api/monitors) and the
// media server is notified by webhook. The one thing nothing
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
	"golang.org/x/sync/errgroup"
)

// defaultResolveConcurrency is the per-pass episode fan-out used when an invalid
// (non-positive) limit is supplied; maxResolveConcurrency is the hard ceiling.
// Both mirror the config clamp so a direct New caller can't exceed the debrid
// safety envelope.
const (
	defaultResolveConcurrency = 4
	maxResolveConcurrency     = 16
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
	Category  string // library root decided for the title; inherited by the pin
}

// Request is an intake from a feeder (the request-shaped API): a movie or
// a whole series, at zero or more requested quality tiers.
type Request struct {
	MediaType string
	IMDbID    string
	TMDbID    string
	TVDbID    string
	Title     string
	Year      int
	Qualities []string
	Seasons   []int // series: requested seasons; empty = all
	// IsAnime, when non-nil, is the authoritative anime flag (e.g. from a Silo
	// request). When nil, the category is derived from a metadata heuristic. It is
	// consulted only at first intake for a title.
	IsAnime *bool
	// RequestRef is an opaque caller key (e.g. a Silo request id) stored on the
	// monitor and echoed by the status API; wisp never interprets it.
	RequestRef string
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
	// resolveConcurrency bounds how many aired episodes of one series resolve in
	// parallel within a pass. It is global (per-title, and titles run one at a
	// time) so it caps the peak debrid fan-out. Always in [1, maxResolveConcurrency].
	resolveConcurrency int
	now                func() time.Time
	wake               chan struct{}
	// nextWakeNano is the unix-nano deadline of the sleep timer the Run loop last
	// armed, published so the schedule API reports the scheduler's real next wake
	// rather than a reconstruction. Zero until the first pass arms a timer.
	nextWakeNano atomic.Int64
	// forceAll, when set, makes the next scheduler pass treat every enabled item
	// as due regardless of its persisted DueAt. It is a one-shot override consumed
	// (swapped back to false) at the start of that pass, so backoff/next-due times
	// resume normally afterward. Set by ForceRefresh (POST /api/monitors/refresh).
	forceAll atomic.Bool
	// mu serializes read-modify-write of a monitored item so the scheduler and a
	// concurrent Intake/delete can't clobber each other (network processing runs
	// outside the lock).
	mu sync.Mutex
}

// New builds a monitor. interval is the fallback re-check ceiling.
// resolveConcurrency bounds per-series episode fan-out per pass; a non-positive
// value defaults to defaultResolveConcurrency and it is capped at
// maxResolveConcurrency.
func New(st *store.Store, meta *metadata.Service, ful Fulfiller, interval time.Duration, resolveConcurrency int, log *slog.Logger) *Monitor {
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	if resolveConcurrency <= 0 {
		resolveConcurrency = defaultResolveConcurrency
	}
	if resolveConcurrency > maxResolveConcurrency {
		resolveConcurrency = maxResolveConcurrency
	}
	return &Monitor{
		store: st, meta: meta, ful: ful, log: log, interval: interval,
		resolveConcurrency: resolveConcurrency,
		now:                time.Now,
		wake:               make(chan struct{}, 1),
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
	key := monitorKey(r.MediaType, r.IMDbID, r.TMDbID)
	searchID := requestSearchID(r)

	item := store.Monitored{
		Key: key, MediaType: r.MediaType,
		IMDbID: r.IMDbID, TMDbID: r.TMDbID, TVDbID: r.TVDbID, Title: r.Title,
		Year: r.Year, Qualities: r.Qualities, Seasons: r.Seasons, DueAt: m.now(),
		Enabled: true, RequestRef: r.RequestRef,
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Decide the category ONCE per title, using only store reads so /api/add never
	// blocks on the network (no synchronous heuristic here). Priority, all
	// first-writer-wins:
	//   1. an existing monitor's category,
	//   2. the category any existing pin already implies (legacy/direct pins),
	//   3. the explicit is_anime flag.
	// With none of those, the category is left empty and the scheduler resolves it
	// (via the heuristic) on its first pass, before any pin path is built.
	cur, _ := m.store.GetMonitored(ctx, item.Key)
	category := ""
	if cur != nil {
		category = cur.Category
	}
	if category == "" {
		category = m.store.CategoryForMedia(ctx, searchID, r.TMDbID)
	}
	switch {
	case category == "" && r.IsAnime != nil:
		category = library.Root(r.MediaType, *r.IsAnime)
	case category != "" && r.IsAnime != nil:
		// A later, conflicting explicit flag never moves an already-categorized
		// title (its pins already live under the stored root).
		if want := library.Root(r.MediaType, *r.IsAnime); want != category {
			m.log.Warn("category conflict; keeping first-intake category",
				"key", key, "stored", category, "requested", want)
		}
	}
	item.Category = category

	// A later request for the same title extends the existing monitor (e.g. a new
	// season, or a 4K request on top of HD) rather than replacing it. A request
	// that scopes no seasons widens coverage to all seasons. Re-requesting resets
	// DueAt to now and clears Completed/Failed so the new work is picked up.
	if cur != nil {
		item.AddedAt = cur.AddedAt
		item.Qualities = unionStrings(cur.Qualities, r.Qualities)
		if len(cur.Seasons) == 0 || len(r.Seasons) == 0 {
			item.Seasons = nil // one unscoped request means "all seasons"
		} else {
			item.Seasons = unionInts(cur.Seasons, r.Seasons)
		}
		if r.RequestRef == "" {
			item.RequestRef = cur.RequestRef // don't blank an existing ref
		}
	}
	if err := m.store.PutMonitored(ctx, item); err != nil {
		return err
	}
	m.log.Info("monitoring", "key", item.Key, "title", item.Title, "category", item.Category)
	m.Wake()
	return nil
}

// ensureCategory resolves and persists a deferred category before any pin path
// is built. It runs on the scheduler (the heuristic here is a network call, kept
// off the /api/add path), and re-checks existing pins first so a title that
// gained legacy/direct pins inherits their root rather than re-deciding. Returns
// the item to process — the freshly-persisted record when a category was set, so
// persistResult's concurrency check still lines up.
func (m *Monitor) ensureCategory(ctx context.Context, it store.Monitored) store.Monitored {
	if it.Category != "" {
		return it
	}
	// Resolve outside the lock (may hit the network); persist under it.
	category := m.store.CategoryForMedia(ctx, monitoredSearchID(it), it.TMDbID)
	if category == "" {
		isAnime := false
		if it.IMDbID != "" {
			isAnime = m.meta.AnimeHeuristic(ctx, it.MediaType, it.IMDbID)
		}
		category = library.Root(it.MediaType, isAnime)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fresh, err := m.store.GetMonitored(ctx, it.Key)
	if err != nil || fresh == nil {
		return it // deleted concurrently
	}
	if fresh.Category != "" {
		return *fresh // a concurrent writer won the race — first-writer-wins
	}
	fresh.Category = category
	if e := m.store.PutMonitored(ctx, *fresh); e != nil {
		m.log.Warn("persist category", "key", fresh.Key, "error", e)
		return it
	}
	updated, _ := m.store.GetMonitored(ctx, it.Key)
	if updated != nil {
		return *updated
	}
	return it
}

// requestSearchID is the id a request's pins are keyed under — imdb if known,
// else "tmdb:<id>" — matching how app.pin stores them.
func requestSearchID(r Request) string {
	if r.IMDbID != "" {
		return r.IMDbID
	}
	if r.TMDbID != "" {
		return "tmdb:" + r.TMDbID
	}
	return ""
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

// Wake nudges the scheduler to run a pass now (e.g. after Intake). It still
// honors each item's persisted DueAt — use it for "something changed, look now".
func (m *Monitor) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// ForceRefresh makes the very next scheduler pass consider every enabled monitor
// due now — running its normal processing (release gate → resolve → pin)
// regardless of persisted DueAt/backoff — then resume normal cadence. It's the
// "operator fixed config, retry everything now" path behind
// POST /api/monitors/refresh, distinct from Wake (which honors DueAt). The
// override is one-shot: it applies to a single pass and does not zero any
// item's stored DueAt.
func (m *Monitor) ForceRefresh() {
	m.forceAll.Store(true)
	m.Wake()
}

// passResult is the outcome of one scheduler pass over a monitored item, folded
// back into the stored record by persistResult.
type passResult struct {
	due          time.Time // next time worth re-checking
	completed    bool      // movie: every requested quality pinned
	reason       string    // one of the store.DueReason* constants
	errMsg       string    // last non-fatal error (surfaced in the API)
	pendingAired int       // series: aired-but-unpinned episodes this pass (0 = caught up)
	failed       bool      // permanent give-up (unresolvable identity)
}

// checkDue processes every due item and returns the earliest next-due time
// across all remaining items (zero if none).
func (m *Monitor) checkDue(ctx context.Context) time.Time {
	items, err := m.store.ListMonitored(ctx)
	if err != nil {
		m.log.Error("list monitored", "error", err)
		return time.Time{}
	}
	// One-shot: a forced refresh treats every enabled item as due for THIS pass
	// only. Consumed here (not before the list, so a transient list error keeps
	// the request pending), and never written to any item's DueAt — normal cadence
	// resumes on the next pass.
	force := m.forceAll.Swap(false)
	now := m.now()
	var earliest time.Time
	for _, it := range items {
		if !it.Enabled || it.Completed || it.Failed {
			continue // paused, a fully-pinned movie kept for history, or given up
		}
		if !force && it.DueAt.After(now) {
			if earliest.IsZero() || it.DueAt.Before(earliest) {
				earliest = it.DueAt
			}
			continue // not due yet
		}
		// Resolve a deferred category before any pin path is built. This is where
		// the (network) heuristic runs — never on the /api/add intake path.
		it = m.ensureCategory(ctx, it)
		// Process outside the lock (it's network-bound); persistResult then folds
		// the result into the current record without clobbering a concurrent
		// Intake or delete.
		var res passResult
		if it.MediaType == "series" {
			res = m.processSeries(ctx, it)
		} else {
			res = m.processMovie(ctx, it)
		}
		if effective := m.persistResult(ctx, it, res); !effective.IsZero() {
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
func (m *Monitor) persistResult(ctx context.Context, snapshot store.Monitored, res passResult) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.store.GetMonitored(ctx, snapshot.Key)
	if err != nil || cur == nil {
		return time.Time{} // deleted concurrently — drop
	}
	cur.LastChecked = m.now()
	cur.LastError = res.errMsg
	if cur.UpdatedAt.Equal(snapshot.UpdatedAt) {
		cur.DueAt = res.due
		cur.DueReason = res.reason
		cur.Failed = res.failed
		if snapshot.MediaType == "series" {
			cur.PendingAired = res.pendingAired
		} else {
			cur.Completed = res.completed
		}
	}
	if e := m.store.PutMonitored(ctx, *cur); e != nil {
		m.log.Warn("update monitored", "key", cur.Key, "error", e)
	}
	if cur.Completed || cur.Failed {
		return time.Time{}
	}
	return cur.DueAt
}

// processMovie pins a released+available movie (marking it Completed) or returns
// when to look again. completed is true once every requested quality is pinned;
// the item is kept (not deleted) so the monitor list is a request history.
func (m *Monitor) processMovie(ctx context.Context, it store.Monitored) passResult {
	now := m.now()
	release, err := m.meta.MovieReleaseDate(ctx, it.IMDbID, it.TMDbID, now)
	switch {
	case errors.Is(err, metadata.ErrNoHomeRelease):
		return passResult{due: now.Add(m.interval), reason: store.DueReasonRetry} // theatrical-only — check again later
	case err != nil:
		m.log.Warn("movie release lookup", "title", it.Title, "error", err)
		return passResult{due: now.Add(m.interval), reason: store.DueReasonRetry, errMsg: err.Error()}
	case release.After(now):
		return passResult{due: release, reason: store.DueReasonRelease} // wake at the real release date
	}
	pinned, err := m.ful.PinnedKeys(ctx, monitoredSearchID(it))
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	if m.pinMissing(ctx, targetsForQualities(it, 0, 0), pinned) == 0 {
		return passResult{completed: true, reason: store.DueReasonRetry} // fully pinned — done, kept for history
	}
	return passResult{due: now.Add(m.interval), reason: store.DueReasonRetry} // released but no stream yet — retry
}

// processSeries pins any aired-but-unpinned episodes and schedules the next wake
// at the next known airstamp. A series is never completed (it may add seasons).
func (m *Monitor) processSeries(ctx context.Context, it store.Monitored) passResult {
	now := m.now()
	all, err := m.meta.Episodes(ctx, it.IMDbID)
	if err != nil {
		if errors.Is(err, metadata.ErrIMDbRequired) {
			// Permanent identity failure — a series can never be enumerated without
			// an imdb id, so give up rather than retry forever.
			m.log.Warn("series enumerate: unresolvable identity", "title", it.Title, "error", err)
			return passResult{reason: store.DueReasonRetry, errMsg: err.Error(), failed: true}
		}
		m.log.Warn("series enumerate", "title", it.Title, "error", err)
		return passResult{due: now.Add(m.interval), reason: store.DueReasonRetry, errMsg: err.Error()}
	}
	if len(it.Seasons) > 0 {
		all = filterSeasons(all, it.Seasons) // honor a per-season request
	}
	pinned, err := m.ful.PinnedKeys(ctx, monitoredSearchID(it))
	if err != nil {
		pinned = map[PinKey]bool{}
	}
	// Resolve aired episodes concurrently, bounded by resolveConcurrency. Each
	// episode is one unit of work touching only its own (season, episode) PinKeys,
	// so the goroutines share the pinned snapshot read-only (no shared mutable
	// dedupe map). Quality tiers stay sequential inside pinMissing so the upstream
	// search cache still collapses a title's tiers into one search.
	var g errgroup.Group
	g.SetLimit(m.resolveConcurrency)
	var remaining atomic.Int64
	for _, ep := range all {
		if ep.Aired.IsZero() || ep.Aired.After(now) {
			continue // not aired yet
		}
		g.Go(func() error {
			// Fold the outcome into an aggregate and always return nil: one
			// episode's resolver hiccup must never fail-fast the whole season.
			remaining.Add(int64(m.pinMissing(ctx, targetsForQualities(it, ep.Season, ep.Number), pinned)))
			return nil
		})
	}
	_ = g.Wait() // workers never error; Wait blocks until the season's episodes finish
	rem := int(remaining.Load())
	nextAir, hasNext := metadata.NextAir(all, now)
	// A stream usually lags an episode's air time (minutes to hours). If an aired
	// episode is still unpinned, retry at the interval — don't defer to the next
	// airstamp, which could be a week (or a mid-season gap) away.
	if rem > 0 {
		retry := now.Add(m.interval)
		if hasNext && nextAir.Before(retry) {
			return passResult{due: nextAir, reason: store.DueReasonAirstamp, pendingAired: rem}
		}
		return passResult{due: retry, reason: store.DueReasonRetry, pendingAired: rem}
	}
	if hasNext {
		return passResult{due: nextAir, reason: store.DueReasonAirstamp} // all aired episodes pinned — wake near the next airing
	}
	return passResult{due: now.Add(m.interval), reason: store.DueReasonRetry} // no known upcoming episode — check again at the ceiling
}

// pinMissing pins every target not already pinned, returning how many remain
// unpinned (0 = fully satisfied). The pinned snapshot is treated as read-only —
// it may be shared across the concurrent per-episode workers — so pins made
// within this call are tracked in a local session map for intra-call dedupe
// (e.g. a default "" tier already satisfied by an earlier tier of the same unit).
func (m *Monitor) pinMissing(ctx context.Context, targets []Target, pinned map[PinKey]bool) (remaining int) {
	var session map[PinKey]bool // pins made in this call; lazily allocated
	for _, t := range targets {
		if isPinned(pinned, t.Season, t.Episode, t.Quality) || isPinned(session, t.Season, t.Episode, t.Quality) {
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
		if session == nil {
			session = make(map[PinKey]bool, len(targets))
		}
		session[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
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
			Category: it.Category,
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

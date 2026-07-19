package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

// fakeFul records pin calls and models existing pins + unavailable episodes.
// Pin is now invoked concurrently (bounded per-episode fan-out), so all mutable
// state is guarded by mu.
type fakeFul struct {
	mu       sync.Mutex
	pinned   map[PinKey]bool
	noStream map[[2]int]bool // (season,episode) with no playable stream
	calls    int
}

func newFakeFul() *fakeFul {
	return &fakeFul{pinned: map[PinKey]bool{}, noStream: map[[2]int]bool{}}
}

func (f *fakeFul) Pin(_ context.Context, t Target) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.noStream[[2]int{t.Season, t.Episode}] {
		return false, nil
	}
	f.pinned[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
	return true, nil
}

func (f *fakeFul) PinnedKeys(_ context.Context, _ string) (map[PinKey]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[PinKey]bool, len(f.pinned))
	for k := range f.pinned {
		out[k] = true
	}
	return out, nil
}

func date(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testMonitor(t *testing.T, mux *http.ServeMux, ful Fulfiller, now time.Time) (*Monitor, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	meta := metadata.New("v3key", []string{"US"}, metadata.WithBaseURLs(srv.URL, srv.URL, srv.URL))
	st := newStore(t)
	m := New(st, meta, ful, time.Hour, 4, slog.New(slog.DiscardHandler))
	m.now = func() time.Time { return now }
	return m, st
}

func TestMonitorPinsReleasedMovieAndMarksComplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)

	if err := m.Intake(context.Background(), Request{MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film", Year: 2026}); err != nil {
		t.Fatal(err)
	}
	m.checkDue(context.Background()) // released → pin + mark complete (kept for history)

	if ful.calls != 1 || len(ful.pinned) != 1 {
		t.Fatalf("expected 1 pin, got calls=%d pinned=%d", ful.calls, len(ful.pinned))
	}
	got, _ := st.GetMonitored(context.Background(), "movie:tt5")
	if got == nil || !got.Completed {
		t.Fatalf("movie should be kept and marked completed; got %#v", got)
	}
	// A completed movie is not reprocessed.
	before := ful.calls
	m.checkDue(context.Background())
	if ful.calls != before {
		t.Fatalf("completed movie reprocessed: calls %d → %d", before, ful.calls)
	}
}

func TestMonitorDefersUnreleasedMovie(t *testing.T) {
	release := "2026-12-25T00:00:00Z"
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"` + release + `"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)

	m.Intake(context.Background(), Request{MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film"})
	next := m.checkDue(context.Background())

	if ful.calls != 0 {
		t.Fatalf("unreleased movie should not pin; calls=%d", ful.calls)
	}
	if !next.Equal(date(release)) {
		t.Fatalf("next wake = %v, want the release date %s", next, release)
	}
	if n, _ := st.CountMonitored(context.Background()); n != 1 {
		t.Fatalf("unreleased movie should stay monitored; monitored=%d", n)
	}
}

func TestMonitorPinsAiredEpisodesAndSchedulesNextAir(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2026-01-08T00:00:00Z"},
			{"season":1,"episode":3,"released":"2026-12-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	ful := newFakeFul()
	now := date("2026-02-01T00:00:00Z")
	m, _ := testMonitor(t, mux, ful, now)

	m.Intake(context.Background(), Request{MediaType: "series", IMDbID: "tt7", Title: "Show", Qualities: []string{"1080p", "2160p"}})
	next := m.checkDue(context.Background())

	// Episodes 1 & 2 aired (2 qualities each = 4 pins); episode 3 is future.
	if len(ful.pinned) != 4 {
		t.Fatalf("expected 4 pins (E1+E2 at 2 tiers), got %d", len(ful.pinned))
	}
	if !next.Equal(date("2026-12-01T00:00:00Z")) {
		t.Fatalf("next wake = %v, want the next airstamp 2026-12-01", next)
	}
	// Second pass must not re-pin already-pinned episodes.
	before := ful.calls
	m.now = func() time.Time { return date("2026-02-02T00:00:00Z") }
	m.checkDue(context.Background())
	if ful.calls != before {
		t.Fatalf("re-pinned already-pinned episodes: calls %d → %d", before, ful.calls)
	}
}

func TestMonitorRecoversFromStore(t *testing.T) {
	// An item persisted before "restart" is picked up by a fresh Monitor.
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now)
	st.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film", DueAt: now, Enabled: true,
	})

	m.checkDue(context.Background()) // fresh monitor, item loaded from store
	if len(ful.pinned) != 1 {
		t.Fatalf("recovered item not processed; pinned=%d", len(ful.pinned))
	}
}

// A just-aired episode whose stream hasn't appeared must be retried at the
// interval, not deferred to the next (possibly distant) airstamp.
func TestMonitorRetriesUnavailableAiredEpisode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2027-06-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	ful := newFakeFul()
	ful.noStream[[2]int{1, 1}] = true // E1 aired but no stream yet
	now := date("2026-02-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now) // interval = 1h

	m.Intake(context.Background(), Request{MediaType: "series", IMDbID: "tt7", Title: "Show", Qualities: []string{"1080p"}})
	m.checkDue(context.Background())

	item, _ := st.GetMonitored(context.Background(), "series:tt7")
	if want := now.Add(time.Hour); !item.DueAt.Equal(want) {
		t.Fatalf("DueAt = %v, want interval retry %v (not the far E2 airstamp)", item.DueAt, want)
	}
}

// persistResult must not clobber a concurrent Intake's changes with a stale
// scheduler snapshot.
func TestPersistResultRespectsConcurrentIntake(t *testing.T) {
	ctx := context.Background()
	m, st := testMonitor(t, http.NewServeMux(), newFakeFul(), date("2026-02-01T00:00:00Z"))
	now := m.now()
	st.PutMonitored(ctx, store.Monitored{Key: "series:tt7", MediaType: "series", IMDbID: "tt7", Enabled: true, Qualities: []string{"1080p"}, DueAt: now})
	snapshot, _ := st.GetMonitored(ctx, "series:tt7")

	// Concurrent Intake: adds a 4K tier and demands reprocessing (DueAt=now).
	st.PutMonitored(ctx, store.Monitored{Key: "series:tt7", MediaType: "series", IMDbID: "tt7", Enabled: true, Qualities: []string{"1080p", "2160p"}, DueAt: now})

	// Scheduler finishes its pass on the STALE snapshot, computing a far DueAt.
	m.persistResult(ctx, *snapshot, passResult{due: now.Add(100 * time.Hour), reason: store.DueReasonRetry})

	cur, _ := st.GetMonitored(ctx, "series:tt7")
	if len(cur.Qualities) != 2 {
		t.Fatalf("concurrent Intake's qualities clobbered: %v", cur.Qualities)
	}
	if cur.DueAt.Equal(now.Add(100 * time.Hour)) {
		t.Fatal("scheduler clobbered the re-request's DueAt with its stale far-future value")
	}
}

// A forced refresh must process an item whose persisted DueAt is in the future
// (e.g. sitting in retry backoff), then let normal cadence resume — the override
// is one-shot and must not linger.
func TestForceRefreshOverridesFutureDueThenResumes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-01-01T00:00:00Z"}]}]}`))
	})
	ful := newFakeFul()
	ful.noStream[[2]int{0, 0}] = true // released, but no stream yet → stays in retry backoff
	now := date("2026-06-01T00:00:00Z")
	m, st := testMonitor(t, mux, ful, now) // interval = 1h

	// A monitored, released movie parked with a far-future DueAt (retry backoff).
	if err := st.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt5", MediaType: "movie", IMDbID: "tt5", TMDbID: "500", Title: "Film",
		Category: library.Root("movie", false), DueAt: now.Add(2 * time.Hour), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Not due yet → an ordinary pass skips it.
	m.checkDue(context.Background())
	if ful.calls != 0 {
		t.Fatalf("future-DueAt item must be skipped without force; calls=%d", ful.calls)
	}

	// A forced refresh treats every enabled item as due now → it's processed.
	m.ForceRefresh()
	m.checkDue(context.Background())
	if ful.calls != 1 {
		t.Fatalf("forced refresh must process the future-DueAt item; calls=%d", ful.calls)
	}

	// The override is one-shot: processing reset DueAt to the retry ceiling
	// (now+interval), NOT zeroed, and the next ordinary pass honors it again.
	got, _ := st.GetMonitored(context.Background(), "movie:tt5")
	if got == nil || !got.DueAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("forced pass should reset DueAt to the retry ceiling now+1h; got %#v", got)
	}
	m.checkDue(context.Background())
	if ful.calls != 1 {
		t.Fatalf("override must not persist; item re-processed after force: calls=%d", ful.calls)
	}
}

func TestMonitorRejectsUngatableMovie(t *testing.T) {
	st := newStore(t)
	m := New(st, metadata.New("", nil), newFakeFul(), time.Hour, 4, slog.New(slog.DiscardHandler)) // no TMDB key
	// tmdb-only movie, no imdb, no TMDB key → no way to gate release.
	if err := m.Intake(context.Background(), Request{MediaType: "movie", TMDbID: "603", Title: "X"}); err == nil {
		t.Fatal("expected rejection of ungatable tmdb-only movie")
	}
	if n, _ := st.CountMonitored(context.Background()); n != 0 {
		t.Fatalf("ungatable movie was stored: %d", n)
	}
}

// instrumentedFul models per-Pin latency and tracks peak concurrent Pin calls so
// tests can assert the bounded fan-out. failEp episodes return an error; noStream
// episodes return (false, nil). All state is guarded for concurrent use.
type instrumentedFul struct {
	latency  time.Duration
	failEp   map[[2]int]bool
	noStream map[[2]int]bool

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	calls       atomic.Int32

	mu     sync.Mutex
	seeded map[PinKey]bool // pre-existing pins reported by PinnedKeys
	pinned map[PinKey]bool // pins created via Pin
}

func newInstrumentedFul(latency time.Duration) *instrumentedFul {
	return &instrumentedFul{
		latency:  latency,
		failEp:   map[[2]int]bool{},
		noStream: map[[2]int]bool{},
		seeded:   map[PinKey]bool{},
		pinned:   map[PinKey]bool{},
	}
}

func (f *instrumentedFul) Pin(_ context.Context, t Target) (bool, error) {
	n := f.inFlight.Add(1)
	for { // publish the running peak
		cur := f.maxInFlight.Load()
		if n <= cur || f.maxInFlight.CompareAndSwap(cur, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	f.calls.Add(1)
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	if f.failEp[[2]int{t.Season, t.Episode}] {
		return false, errors.New("resolver hiccup")
	}
	if f.noStream[[2]int{t.Season, t.Episode}] {
		return false, nil
	}
	f.mu.Lock()
	f.pinned[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
	f.mu.Unlock()
	return true, nil
}

func (f *instrumentedFul) PinnedKeys(_ context.Context, _ string) (map[PinKey]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[PinKey]bool{}
	for k := range f.seeded {
		out[k] = true
	}
	for k := range f.pinned {
		out[k] = true
	}
	return out, nil
}

// seriesEpisodesMux serves a Cinemeta series with n episodes, all aired in the
// past relative to the test clock, so processSeries resolves every one.
func seriesEpisodesMux(imdb string, n int) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/"+imdb+".json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[`))
		for i := 1; i <= n; i++ {
			if i > 1 {
				w.Write([]byte(","))
			}
			fmt.Fprintf(w, `{"season":1,"episode":%d,"released":"2026-01-01T00:00:00Z"}`, i)
		}
		w.Write([]byte(`]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":0}`)) })
	return mux
}

func seriesItem(imdb string, qualities ...string) store.Monitored {
	return store.Monitored{
		Key: "series:" + imdb, MediaType: "series", IMDbID: imdb,
		Qualities: qualities, Enabled: true, Category: library.Root("series", false),
	}
}

// A season resolves its aired episodes in parallel, but never above the limit:
// peak concurrent Pin calls stays ≤ resolveConcurrency, and total wall-clock
// reflects the parallelism (≈ ceil(n/limit) waves, not n sequential).
func TestSeriesResolvesEpisodesWithBoundedConcurrency(t *testing.T) {
	const (
		episodes = 8
		limit    = 4
		latency  = 50 * time.Millisecond
	)
	ful := newInstrumentedFul(latency)
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", episodes), ful, now)
	m.resolveConcurrency = limit

	start := time.Now()
	res := m.processSeries(context.Background(), seriesItem("tt7", "1080p"))
	elapsed := time.Since(start)

	if got := ful.calls.Load(); got != episodes {
		t.Fatalf("Pin calls = %d, want %d (one per aired episode)", got, episodes)
	}
	if res.pendingAired != 0 {
		t.Fatalf("pendingAired = %d, want 0 (every episode resolved)", res.pendingAired)
	}
	if peak := ful.maxInFlight.Load(); peak > limit {
		t.Fatalf("peak concurrent Pin = %d, exceeds limit %d", peak, limit)
	} else if peak < 2 {
		t.Fatalf("peak concurrent Pin = %d, expected real parallelism (>1)", peak)
	}
	// Sequential would be episodes*latency = 400ms; ~2 waves is ~100ms. Allow
	// generous slack for a loaded CI while still proving it isn't serial.
	if maxWall := time.Duration(episodes) * latency; elapsed >= maxWall {
		t.Fatalf("wall-clock %v ≥ sequential floor %v — not parallel", elapsed, maxWall)
	}
}

// The aggregate pendingAired from the parallel path must equal the sequential
// (limit=1) result for a mix of already-pinned, freshly-pinned, and no-stream
// episodes.
func TestSeriesAggregateMatchesSequential(t *testing.T) {
	// E2 already pinned (dedupe → 0), E3 & E5 have no stream (→ remaining),
	// the rest pin cleanly (→ 0). Expected pendingAired = 2, both runs.
	build := func() *instrumentedFul {
		f := newInstrumentedFul(0)
		f.seeded[PinKey{1, 2, "1080p"}] = true
		f.noStream[[2]int{1, 3}] = true
		f.noStream[[2]int{1, 5}] = true
		return f
	}

	run := func(limit int) int {
		ful := build()
		now := date("2026-06-01T00:00:00Z")
		m, _ := testMonitor(t, seriesEpisodesMux("tt7", 6), ful, now)
		m.resolveConcurrency = limit
		return m.processSeries(context.Background(), seriesItem("tt7", "1080p")).pendingAired
	}

	seq := run(1)
	par := run(4)
	if seq != 2 {
		t.Fatalf("sequential pendingAired = %d, want 2", seq)
	}
	if par != seq {
		t.Fatalf("parallel pendingAired = %d != sequential %d", par, seq)
	}
}

// One episode's resolver error must not abort the rest of the season: every
// other aired episode is still processed, and only the failed unit is counted
// as remaining.
func TestSeriesEpisodeErrorDoesNotAbortOthers(t *testing.T) {
	ful := newInstrumentedFul(0)
	ful.failEp[[2]int{1, 3}] = true // E3 errors on Pin
	now := date("2026-06-01T00:00:00Z")
	m, _ := testMonitor(t, seriesEpisodesMux("tt7", 6), ful, now)
	m.resolveConcurrency = 4

	res := m.processSeries(context.Background(), seriesItem("tt7", "1080p"))

	if got := ful.calls.Load(); got != 6 {
		t.Fatalf("Pin calls = %d, want 6 (error must not short-circuit the season)", got)
	}
	if res.pendingAired != 1 {
		t.Fatalf("pendingAired = %d, want 1 (only the failed episode)", res.pendingAired)
	}
	for _, ep := range []int{1, 2, 4, 5, 6} {
		if !ful.pinned[PinKey{1, ep, "1080p"}] {
			t.Fatalf("episode %d was not pinned despite E3 failing", ep)
		}
	}
}

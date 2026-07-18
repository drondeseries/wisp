package monitor

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

// fakeFul records pin calls and models existing pins + unavailable episodes.
type fakeFul struct {
	pinned   map[PinKey]bool
	noStream map[[2]int]bool // (season,episode) with no playable stream
	calls    int
}

func newFakeFul() *fakeFul {
	return &fakeFul{pinned: map[PinKey]bool{}, noStream: map[[2]int]bool{}}
}

func (f *fakeFul) Pin(_ context.Context, t Target) (bool, error) {
	f.calls++
	if f.noStream[[2]int{t.Season, t.Episode}] {
		return false, nil
	}
	f.pinned[PinKey{t.Season, t.Episode, library.NormalizeQuality(t.Quality)}] = true
	return true, nil
}

func (f *fakeFul) PinnedKeys(_ context.Context, _ string) (map[PinKey]bool, error) {
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
	m := New(st, meta, ful, time.Hour, slog.New(slog.DiscardHandler))
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

func TestMonitorRejectsUngatableMovie(t *testing.T) {
	st := newStore(t)
	m := New(st, metadata.New("", nil), newFakeFul(), time.Hour, slog.New(slog.DiscardHandler)) // no TMDB key
	// tmdb-only movie, no imdb, no TMDB key → no way to gate release.
	if err := m.Intake(context.Background(), Request{MediaType: "movie", TMDbID: "603", Title: "X"}); err == nil {
		t.Fatal("expected rejection of ungatable tmdb-only movie")
	}
	if n, _ := st.CountMonitored(context.Background()); n != 0 {
		t.Fatalf("ungatable movie was stored: %d", n)
	}
}

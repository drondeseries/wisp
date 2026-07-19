package monitor

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/store"
)

func ptrBool(b bool) *bool { return &b }

// An explicit is_anime flag is authoritative at first intake and fixes the root.
func TestIntakeExplicitAnimeFlag(t *testing.T) {
	ctx := context.Background()
	m, st := testMonitor(t, http.NewServeMux(), newFakeFul(), date("2026-01-01T00:00:00Z"))

	if err := m.Intake(ctx, Request{MediaType: "movie", IMDbID: "tt5", Title: "Akira", IsAnime: ptrBool(true)}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMonitored(ctx, "movie:tt5")
	if got == nil || got.Category != "anime_movies" {
		t.Fatalf("category = %v, want anime_movies", got)
	}
}

// With no explicit flag, the category is deferred at intake (no synchronous
// heuristic) and resolved by the scheduler's first pass via the Cinemeta
// heuristic: Animation + Japanese → anime.
func TestIntakeHeuristicCategory(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"genres":["Animation"],"country":"Japan"}}`))
	})
	m, st := testMonitor(t, mux, newFakeFul(), date("2026-01-01T00:00:00Z"))

	if err := m.Intake(ctx, Request{MediaType: "series", IMDbID: "tt7", Title: "Frieren"}); err != nil {
		t.Fatal(err)
	}
	// Intake defers: no heuristic call, category unresolved.
	got, _ := st.GetMonitored(ctx, "series:tt7")
	if got == nil || got.Category != "" {
		t.Fatalf("category after intake = %v, want empty (deferred)", got)
	}
	// The scheduler pass resolves it via the heuristic before any pin.
	m.checkDue(ctx)
	got, _ = st.GetMonitored(ctx, "series:tt7")
	if got == nil || got.Category != "anime_shows" {
		t.Fatalf("category = %v, want anime_shows (heuristic on scheduler pass)", got)
	}
}

// An explicit flag wins over what the heuristic would say — even when Cinemeta
// reports anime, is_anime:false keeps the title in the non-anime root.
func TestIntakeExplicitFlagBeatsHeuristic(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt7.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"genres":["Animation"],"country":"Japan"}}`))
	})
	m, st := testMonitor(t, mux, newFakeFul(), date("2026-01-01T00:00:00Z"))

	if err := m.Intake(ctx, Request{MediaType: "series", IMDbID: "tt7", Title: "Not Anime", IsAnime: ptrBool(false)}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMonitored(ctx, "series:tt7")
	if got == nil || got.Category != "shows" {
		t.Fatalf("category = %v, want shows (explicit flag beats heuristic)", got)
	}
}

// First-monitor intake for a title that already has legacy/direct pins inherits
// the pins' category rather than re-deciding from a flag — a conflicting flag
// would otherwise split the title across roots (violating first-writer-wins).
func TestIntakeInheritsCategoryFromExistingPins(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	st := newStore(t)
	m := New(st, metadata.New("", nil), newFakeFul(), time.Hour, 4, log)
	m.now = func() time.Time { return date("2026-01-01T00:00:00Z") }

	// A legacy/direct pin already decided anime for this title.
	if err := st.Upsert(ctx, store.Pin{
		MediaType: "series", IMDbID: "tt7", TMDbID: "555", Category: "anime_shows", Quality: "1080p",
		VirtualPath: "anime_shows/Show (2026)/Season 01/e.mkv", SourceURL: "http://a", Size: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// A conflicting explicit is_anime:false must NOT move the title.
	if err := m.Intake(ctx, Request{MediaType: "series", IMDbID: "tt7", Title: "Show", IsAnime: ptrBool(false)}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMonitored(ctx, "series:tt7")
	if got == nil || got.Category != "anime_shows" {
		t.Fatalf("category = %v, want anime_shows (inherited from existing pin)", got)
	}
	if !strings.Contains(buf.String(), "category conflict") {
		t.Fatalf("expected a category-conflict warning, log = %q", buf.String())
	}
}

// First-writer-wins: a later, conflicting is_anime never moves the title, and
// the conflict is logged.
func TestIntakeFirstWriterWins(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	st := newStore(t)
	// Both intakes carry explicit flags, so the heuristic (network) never runs.
	m := New(st, metadata.New("", nil), newFakeFul(), time.Hour, 4, log)
	m.now = func() time.Time { return date("2026-01-01T00:00:00Z") }

	if err := m.Intake(ctx, Request{MediaType: "movie", IMDbID: "tt5", Title: "X", IsAnime: ptrBool(false)}); err != nil {
		t.Fatal(err)
	}
	// Conflicting re-request: now claims anime.
	if err := m.Intake(ctx, Request{MediaType: "movie", IMDbID: "tt5", Title: "X", IsAnime: ptrBool(true)}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetMonitored(ctx, "movie:tt5")
	if got == nil || got.Category != "movies" {
		t.Fatalf("category = %v, want movies (first write wins)", got)
	}
	if !strings.Contains(buf.String(), "category conflict") {
		t.Fatalf("expected a category-conflict warning, log = %q", buf.String())
	}
}

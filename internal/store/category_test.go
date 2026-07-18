package store

import (
	"context"
	"testing"
	"time"
)

// A pin round-trips its new identity/category fields unchanged.
func TestPinTMDBAndCategoryRoundTrip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	p := Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205", TVDbID: "12345",
		Category: "movies", Title: "Inception", Year: 2010, Quality: "2160p",
		VirtualPath: "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [2160p].mkv",
		SourceURL:   "http://a", Size: 100, ResolvedAt: time.Unix(1000, 0),
	}
	if err := st.Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.ByPath(ctx, p.VirtualPath)
	if err != nil || got == nil {
		t.Fatalf("ByPath = %v, %v", got, err)
	}
	if got.TMDbID != "27205" || got.TVDbID != "12345" || got.Category != "movies" {
		t.Fatalf("round-trip lost fields: %#v", got)
	}
}

// Backfill stamps Category on pre-category records from their existing path,
// WITHOUT rewriting the VirtualPath (the bbolt key / on-disk location), and is
// idempotent.
func TestBackfillCategoriesAdditive(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	// Two legacy pins with no Category (as if written before the field existed).
	moviePath := "movies/Old Film (2019)/Old Film (2019) - [1080p].mkv"
	showPath := "shows/Old Show (2018)/Season 01/Old Show (2018) - S01E01 - [1080p].mkv"
	if err := st.Upsert(ctx, Pin{MediaType: "movie", IMDbID: "tt-movie", VirtualPath: moviePath, SourceURL: "http://x", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(ctx, Pin{MediaType: "series", IMDbID: "tt-show", VirtualPath: showPath, SourceURL: "http://x", Size: 1}); err != nil {
		t.Fatal(err)
	}
	// A legacy monitor with no Category, matching the movie pin's id.
	if err := st.PutMonitored(ctx, Monitored{Key: "movie:tt-movie", MediaType: "movie", IMDbID: "tt-movie", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A legacy monitor with no pins → falls back to the non-anime default.
	if err := st.PutMonitored(ctx, Monitored{Key: "series:tt-orphan", MediaType: "series", IMDbID: "tt-orphan", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	n, err := st.BackfillCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // 2 pins + 2 monitors
		t.Fatalf("backfilled %d records, want 4", n)
	}

	// Pins: Category stamped, VirtualPath byte-identical.
	mv, _ := st.ByPath(ctx, moviePath)
	if mv == nil || mv.Category != "movies" || mv.VirtualPath != moviePath {
		t.Fatalf("movie pin backfill = %#v", mv)
	}
	sh, _ := st.ByPath(ctx, showPath)
	if sh == nil || sh.Category != "shows" || sh.VirtualPath != showPath {
		t.Fatalf("show pin backfill = %#v", sh)
	}

	// Monitor inherits from its pin; orphan monitor defaults non-anime.
	mon, _ := st.GetMonitored(ctx, "movie:tt-movie")
	if mon == nil || mon.Category != "movies" {
		t.Fatalf("monitor inherit = %#v", mon)
	}
	orphan, _ := st.GetMonitored(ctx, "series:tt-orphan")
	if orphan == nil || orphan.Category != "shows" {
		t.Fatalf("orphan monitor default = %#v", orphan)
	}

	// Idempotent: a second pass changes nothing.
	if n2, err := st.BackfillCategories(ctx); err != nil || n2 != 0 {
		t.Fatalf("second backfill = %d (err %v), want 0", n2, err)
	}
}

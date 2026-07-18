package store

import (
	"context"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertAndByPath(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	p := Pin{
		MediaType: "series", IMDbID: "tt1", Season: 1, Episode: 4, Title: "Show", Year: 2026,
		Quality: "1080p", VirtualPath: "shows/Show/Season 01/ep.mkv",
		SourceURL: "http://a", Size: 100, ResolvedAt: time.Unix(1000, 0),
	}
	if err := st.Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := st.ByPath(ctx, p.VirtualPath)
	if err != nil || got == nil {
		t.Fatalf("ByPath = %v, %v", got, err)
	}
	if got.SourceURL != "http://a" || got.Size != 100 {
		t.Fatalf("got %#v", got)
	}

	// Upsert on same virtual path replaces resolution, not identity.
	p.SourceURL, p.Size = "http://b", 200
	if err := st.Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.ByPath(ctx, p.VirtualPath)
	if got2.ID != got.ID {
		t.Fatalf("upsert changed id: %d -> %d", got.ID, got2.ID)
	}
	if got2.SourceURL != "http://b" || got2.Size != 200 {
		t.Fatalf("upsert did not replace: %#v", got2)
	}
}

func TestByPathMissing(t *testing.T) {
	st := open(t)
	got, err := st.ByPath(context.Background(), "nope")
	if err != nil || got != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", got, err)
	}
}

func TestUpdateResolution(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	p := Pin{MediaType: "movie", IMDbID: "tt1", Title: "M", Year: 2020, Quality: "1080p",
		VirtualPath: "movies/M/m.mkv", SourceURL: "http://old", Size: 1, ResolvedAt: time.Now()}
	st.Upsert(ctx, p)
	if err := st.UpdateResolution(ctx, p.VirtualPath, "http://new", 999); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.ByPath(ctx, p.VirtualPath)
	if got2.SourceURL != "http://new" || got2.Size != 999 {
		t.Fatalf("update failed: %#v", got2)
	}
}

func TestChildrenSynthesizesTree(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	paths := []string{
		"shows/Show A (2026)/Season 01/e1.mkv",
		"shows/Show A (2026)/Season 01/e2.mkv",
		"shows/Show B (2025)/Season 02/e1.mkv",
		"movies/Film (2020)/film.mkv",
	}
	for i, vp := range paths {
		st.Upsert(ctx, Pin{MediaType: "series", IMDbID: "tt", VirtualPath: vp,
			SourceURL: "http://x", Size: int64(i + 1), ResolvedAt: time.Now()})
	}

	dirs, files, err := st.Children(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || len(files) != 0 { // movies/, shows/
		t.Fatalf("root: dirs=%v files=%v", dirs, files)
	}

	dirs, _, _ = st.Children(ctx, "shows")
	if len(dirs) != 2 { // Show A, Show B
		t.Fatalf("shows dirs = %v", dirs)
	}

	dirs, files, _ = st.Children(ctx, "shows/Show A (2026)/Season 01")
	if len(dirs) != 0 || len(files) != 2 {
		t.Fatalf("season: dirs=%v files=%v", dirs, files)
	}
}

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

func TestListCountDelete(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	mk := func(vp, imdb string, s, e int) Pin {
		return Pin{MediaType: "series", IMDbID: imdb, Season: s, Episode: e, VirtualPath: vp,
			SourceURL: "http://x", Size: 1, ResolvedAt: time.Now()}
	}
	st.Upsert(ctx, mk("shows/A/S01/e1.mkv", "tt1", 1, 1))
	st.Upsert(ctx, mk("shows/A/S01/e2.mkv", "tt1", 1, 2))
	st.Upsert(ctx, mk("movies/B/b.mkv", "tt2", 0, 0))

	if n, _ := st.Count(ctx); n != 3 {
		t.Fatalf("count = %d", n)
	}
	pins, _ := st.List(ctx)
	if len(pins) != 3 {
		t.Fatalf("list = %d", len(pins))
	}

	existed, _ := st.Delete(ctx, "movies/B/b.mkv")
	if !existed {
		t.Fatal("delete reported not-existed")
	}
	if n, _ := st.Count(ctx); n != 2 {
		t.Fatalf("count after delete = %d", n)
	}

	deleted, _ := st.DeleteByMedia(ctx, "tt1", 1, 1, "")
	if len(deleted) != 1 || deleted[0] != "shows/A/S01/e1.mkv" {
		t.Fatalf("delete by media = %v", deleted)
	}
	if n, _ := st.Count(ctx); n != 1 {
		t.Fatalf("count after media delete = %d", n)
	}
}

func TestDeleteByMediaQualityFilter(t *testing.T) {
	ctx := context.Background()
	st := open(t)

	for _, q := range []string{"1080p", "2160p"} {
		if err := st.Upsert(ctx, Pin{
			IMDbID: "tt9", MediaType: "movie", Quality: q,
			VirtualPath: "movies/Film (2026) - [" + q + "].mkv",
		}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := st.DeleteByMedia(ctx, "tt9", 0, 0, "2160p")
	if err != nil || len(deleted) != 1 {
		t.Fatalf("quality-scoped delete = %v (err %v), want one path", deleted, err)
	}
	if n, _ := st.Count(ctx); n != 1 {
		t.Fatalf("count after quality delete = %d, want 1 (1080p kept)", n)
	}
	if p, _ := st.ByPath(ctx, "movies/Film (2026) - [1080p].mkv"); p == nil {
		t.Fatal("1080p pin should remain")
	}
}

// A pin saved under a non-canonical label ("4K", e.g. from an older version)
// must still be removable by its canonical quality ("2160p").
func TestDeleteByMediaNormalizesStoredLabel(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	if err := st.Upsert(ctx, Pin{
		IMDbID: "tt4", MediaType: "movie", Quality: "4K",
		VirtualPath: "movies/Old (2020) - [4K].mkv",
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteByMedia(ctx, "tt4", 0, 0, "2160p")
	if err != nil || len(deleted) != 1 {
		t.Fatalf("delete = %v (err %v), want the 4K pin matched by 2160p", deleted, err)
	}
}

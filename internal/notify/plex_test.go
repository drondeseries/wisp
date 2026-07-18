package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const plexSectionsBody = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="2">
  <Directory key="1" type="movie" title="Movies">
    <Location id="1" path="/mnt/wisp/movies"/>
  </Directory>
  <Directory key="2" type="show" title="Shows">
    <Location id="2" path="/mnt/wisp/shows"/>
  </Directory>
</MediaContainer>`

type plexRefresh struct {
	section string
	path    string
	token   string
}

func plexServer(t *testing.T, refreshes *[]plexRefresh, mu *sync.Mutex, sectionHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/sections":
			mu.Lock()
			*sectionHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(plexSectionsBody))
		case len(r.URL.Path) > len("/library/sections/") && r.URL.Path[len(r.URL.Path)-len("/refresh"):] == "/refresh":
			// /library/sections/{key}/refresh
			key := r.URL.Path[len("/library/sections/") : len(r.URL.Path)-len("/refresh")]
			mu.Lock()
			*refreshes = append(*refreshes, plexRefresh{
				section: key, path: r.URL.Query().Get("path"), token: r.Header.Get("X-Plex-Token"),
			})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected plex request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestPlexRefreshesFolderOfChangedFile(t *testing.T) {
	var (
		refreshes   []plexRefresh
		mu          sync.Mutex
		sectionHits int
	)
	server := plexServer(t, &refreshes, &mu, &sectionHits)
	defer server.Close()

	tgt := newPlexTarget(server.URL, "plex-token", "/mnt/wisp", slog.New(slog.DiscardHandler))

	tgt.Import(context.Background(), "series", "shows/Demo (2026)/Season 01/Demo S01E01.mkv")
	tgt.Delete(context.Background(), "movie", "movies/Inception (2010)/Inception.mkv")

	if len(refreshes) != 2 {
		t.Fatalf("refresh count = %d: %#v", len(refreshes), refreshes)
	}
	// Series episode → shows section (key 2), folder = the season directory.
	if refreshes[0].section != "2" {
		t.Fatalf("series section = %s, want 2", refreshes[0].section)
	}
	if refreshes[0].path != "/mnt/wisp/shows/Demo (2026)/Season 01" {
		t.Fatalf("series refresh path = %q", refreshes[0].path)
	}
	if refreshes[0].token != "plex-token" {
		t.Fatalf("token = %q", refreshes[0].token)
	}
	// Movie → movies section (key 1), folder = the movie directory.
	if refreshes[1].section != "1" {
		t.Fatalf("movie section = %s, want 1", refreshes[1].section)
	}
	if refreshes[1].path != "/mnt/wisp/movies/Inception (2010)" {
		t.Fatalf("movie refresh path = %q", refreshes[1].path)
	}
	// Sections are cached: only one discovery call for two events.
	if sectionHits != 1 {
		t.Fatalf("section fetches = %d, want 1 (cached)", sectionHits)
	}
}

func TestPlexRenameDedupesSharedFolder(t *testing.T) {
	var (
		refreshes   []plexRefresh
		mu          sync.Mutex
		sectionHits int
	)
	server := plexServer(t, &refreshes, &mu, &sectionHits)
	defer server.Close()

	tgt := newPlexTarget(server.URL, "", "/mnt/wisp", slog.New(slog.DiscardHandler))
	// Both paths live in the same movie folder → one refresh.
	tgt.Rename(context.Background(), "movie",
		"movies/Inception (2010)/Inception [1080p].mkv",
		"movies/Inception (2010)/Inception [2160p].mkv")

	if len(refreshes) != 1 {
		t.Fatalf("refresh count = %d, want 1 (same folder deduped)", len(refreshes))
	}
	if refreshes[0].path != "/mnt/wisp/movies/Inception (2010)" {
		t.Fatalf("refresh path = %q", refreshes[0].path)
	}
}

func TestParsePlexSectionsAndLongestPrefix(t *testing.T) {
	sections, err := parsePlexSections([]byte(plexSectionsBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 {
		t.Fatalf("sections = %d", len(sections))
	}

	cases := []struct {
		dir     string
		wantKey string
		wantOK  bool
	}{
		{"/mnt/wisp/movies/Inception (2010)", "1", true},
		{"/mnt/wisp/shows/Demo/Season 01", "2", true},
		{"/mnt/wisp/movies", "1", true},
		{"/mnt/other/movies", "", false},
		{"/mnt/wisp/moviesX/foo", "", false}, // must not prefix-match /mnt/wisp/movies
	}
	for _, tc := range cases {
		key, ok := sectionForPath(sections, tc.dir)
		if ok != tc.wantOK || key != tc.wantKey {
			t.Fatalf("sectionForPath(%q) = (%q,%v), want (%q,%v)", tc.dir, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}

// A section whose location is the filesystem root ("/") must cover every
// absolute path, and a more specific section must still win over it.
func TestPlexRootLocationCoversAllPaths(t *testing.T) {
	sections := []plexSection{
		{Key: "1", Locations: []string{"/"}},
		{Key: "2", Locations: []string{"/mnt/wisp/movies"}},
	}
	// A path only the root section covers.
	if key, ok := sectionForPath(sections, "/mnt/wisp/shows/Demo/Season 01"); !ok || key != "1" {
		t.Fatalf("root coverage = (%q,%v), want (1,true)", key, ok)
	}
	// A more specific section still wins over root.
	if key, ok := sectionForPath(sections, "/mnt/wisp/movies/Inception (2010)"); !ok || key != "2" {
		t.Fatalf("specific-over-root = (%q,%v), want (2,true)", key, ok)
	}
}

func TestPlexRootSectionParsedFromXML(t *testing.T) {
	body := `<?xml version="1.0"?>
<MediaContainer>
  <Directory key="1" title="Everything"><Location path="/"/></Directory>
</MediaContainer>`
	sections, err := parsePlexSections([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := sectionForPath(sections, "/data/movies/Film"); !ok || key != "1" {
		t.Fatalf("root section from XML = (%q,%v), want (1,true)", key, ok)
	}
}

func TestPlexLongestPrefixWinsNestedSections(t *testing.T) {
	sections := []plexSection{
		{Key: "1", Locations: []string{"/data"}},
		{Key: "2", Locations: []string{"/data/movies"}},
	}
	key, ok := sectionForPath(sections, "/data/movies/Inception (2010)")
	if !ok || key != "2" {
		t.Fatalf("nested prefix = (%q,%v), want (2,true)", key, ok)
	}
}

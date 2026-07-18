package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnimeHeuristic covers the minimal, conservative anime signal: Animation
// genre AND a Japanese language/country. Western animation and genre-only titles
// stay non-anime; a failed/absent lookup defaults false.
func TestAnimeHeuristic(t *testing.T) {
	mux := http.NewServeMux()
	serve := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) })
	}
	serve("/meta/series/tt-jp-lang.json", `{"meta":{"genres":["Animation","Action"],"language":"ja"}}`)
	serve("/meta/series/tt-jp-country.json", `{"meta":{"genres":["Animation"],"country":"Japan"}}`)
	serve("/meta/movie/tt-western.json", `{"meta":{"genres":["Animation","Comedy"],"language":"en","country":"United States"}}`)
	serve("/meta/movie/tt-genre-only.json", `{"meta":{"genres":["Animation"]}}`)
	serve("/meta/series/tt-live-action.json", `{"meta":{"genres":["Drama"],"country":"Japan"}}`)

	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := New("", nil, WithBaseURLs(srv.URL, srv.URL, srv.URL))

	cases := []struct {
		name      string
		mediaType string
		imdbID    string
		want      bool
	}{
		{"animation + japanese language", "series", "tt-jp-lang", true},
		{"animation + japan country", "series", "tt-jp-country", true},
		{"western animation", "movie", "tt-western", false},
		{"animation genre only (no jp signal)", "movie", "tt-genre-only", false},
		{"japanese live action", "series", "tt-live-action", false},
		{"unknown id (lookup fails)", "movie", "tt-missing", false},
		{"non-imdb id", "movie", "tmdb:123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.AnimeHeuristic(context.Background(), tc.mediaType, tc.imdbID); got != tc.want {
				t.Fatalf("AnimeHeuristic(%q,%q) = %v, want %v", tc.mediaType, tc.imdbID, got, tc.want)
			}
		})
	}
}

package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/meta/series/tt38262097.json":
			w.Write([]byte(`{"meta":{"tvdb_id":467127,"moviedb_id":298994}}`))
		case "/meta/movie/tt1375666.json":
			w.Write([]byte(`{"meta":{"moviedb_id":27205}}`)) // no tvdb for movies
		case "/meta/movie/tt0.json":
			w.Write([]byte(`{"meta":{}}`)) // nothing mapped
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	old := cinemetaBase
	cinemetaBase = server.URL
	defer func() { cinemetaBase = old }()

	if tvdb, tmdb := ProviderIDs(context.Background(), "series", "tt38262097"); tvdb != "467127" || tmdb != "298994" {
		t.Fatalf("series ids = %q, %q", tvdb, tmdb)
	}
	if tvdb, tmdb := ProviderIDs(context.Background(), "movie", "tt1375666"); tvdb != "" || tmdb != "27205" {
		t.Fatalf("movie ids = %q, %q", tvdb, tmdb)
	}
	if tvdb, tmdb := ProviderIDs(context.Background(), "movie", "tt0"); tvdb != "" || tmdb != "" {
		t.Fatalf("unmapped ids = %q, %q", tvdb, tmdb)
	}
}

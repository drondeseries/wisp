package aiostreams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeriveCredentials(t *testing.T) {
	cases := []struct {
		name, url, password, want string
	}{
		{"blob url", "https://h/stremio/uuid-1/blob/manifest.json", "pw", "uuid-1:pw"},
		{"alias url", "https://h/stremio/u/spoked/manifest.json", "pw", "spoked:pw"},
		{"verbatim creds", "https://h/stremio/uuid-1/blob/manifest.json", "user:secret", "user:secret"},
		{"no password", "https://h/stremio/uuid-1/blob/manifest.json", "", "uuid-1"},
		{"no stremio segment", "https://h/manifest.json", "pw", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveCredentials(c.url, c.password); got != c.want {
				t.Fatalf("deriveCredentials() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSearchParsesResults proves the client builds the right request and keeps
// only results carrying a playable URL — regardless of transport (debrid or
// usenet), which is opaque to wisp.
func TestSearchParsesResults(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"success":true,"data":{"results":[
			{"url":"https://cdn.example/dl/abc/Show.S01E04.1080p.mkv","name":"Debrid 1080p"},
			{"url":"https://aio.example/usenet/stream/xyz/Show.S01E04.mkv","name":"Usenet 1080p"},
			{"url":"","name":"no url, dropped"}
		]}}`))
	}))
	defer server.Close()

	c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")
	streams, err := c.Search(context.Background(), "series", "tt38262097", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2 (empty-url dropped)", len(streams))
	}
	if streams[0].Filename != "Show.S01E04.1080p.mkv" {
		t.Fatalf("debrid filename = %q", streams[0].Filename)
	}
	if streams[1].Filename != "Show.S01E04.mkv" {
		t.Fatalf("usenet filename = %q", streams[1].Filename)
	}
	if wantPath := "/api/v1/search?id=tt38262097%3A1%3A4&requiredFields=url&type=series"; gotPath != wantPath {
		t.Fatalf("request path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth == "" {
		t.Fatal("no basic auth sent")
	}
}

func TestSearchMovieID(t *testing.T) {
	var gotID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.URL.Query().Get("id")
		w.Write([]byte(`{"success":true,"data":{"results":[]}}`))
	}))
	defer server.Close()
	c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")
	if _, err := c.Search(context.Background(), "movie", "tt123", 0, 0); err != nil {
		t.Fatal(err)
	}
	if gotID != "tt123" {
		t.Fatalf("movie id = %q, want tt123 (no season/episode)", gotID)
	}
}

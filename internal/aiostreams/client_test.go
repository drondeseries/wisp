package aiostreams

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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

// TestSearchClassifiesFailures proves upstream failures are typed so callers can
// tell a config/throttle problem from a genuine no-stream condition.
func TestSearchClassifiesFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		wantKind   ErrorKind
		wantRetry  time.Duration
	}{
		{"unauthorized", http.StatusUnauthorized, "", KindAuth, 0},
		{"forbidden", http.StatusForbidden, "", KindAuth, 0},
		{"rate limited", http.StatusTooManyRequests, "12", KindRateLimited, 12 * time.Second},
		{"server error", http.StatusBadGateway, "", KindTransient, 0},
		{"teapot", http.StatusTeapot, "", KindUpstream, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")
			_, err := c.Search(context.Background(), "movie", "tt123", 0, 0)
			var se *SearchError
			if !errors.As(err, &se) {
				t.Fatalf("error = %v, want *SearchError", err)
			}
			if se.Kind != tc.wantKind {
				t.Fatalf("kind = %d, want %d", se.Kind, tc.wantKind)
			}
			if se.RetryAfter != tc.wantRetry {
				t.Fatalf("retryAfter = %s, want %s", se.RetryAfter, tc.wantRetry)
			}
		})
	}
}

// TestSearchTransportFailureIsTransient proves an unreachable upstream is a
// transient error, not a no-stream condition.
func TestSearchTransportFailureIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")
	server.Close() // now unreachable

	_, err := c.Search(context.Background(), "movie", "tt123", 0, 0)
	var se *SearchError
	if !errors.As(err, &se) || se.Kind != KindTransient {
		t.Fatalf("error = %v, want transient SearchError", err)
	}
}

func TestHasCredentials(t *testing.T) {
	if !New("https://h/stremio/uuid-1/blob/manifest.json", "pw").HasCredentials() {
		t.Fatal("uuid + password should have credentials")
	}
	if New("https://h/stremio/uuid-1/blob/manifest.json", "").HasCredentials() {
		t.Fatal("uuid without password cannot authenticate")
	}
}

// AIOStreams reports bad credentials as HTTP 400 with a structured error code,
// not 401 — that must still classify as auth, not a transient upstream fault.
func TestSearchClassifiesInvalidCredentials400(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"success":false,"error":{"code":"USER_INVALID_DETAILS","message":"Invalid UUID or password"}}`))
	}))
	defer server.Close()

	c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "wrong")
	_, err := c.Search(context.Background(), "movie", "tt1", 0, 0)
	var se *SearchError
	if !errors.As(err, &se) || se.Kind != KindAuth {
		t.Fatalf("error = %v, want KindAuth for USER_INVALID_DETAILS", err)
	}
}

// Within the TTL, repeated Search calls for the same unit hit AIOStreams once —
// the cached result set serves every requested quality tier. A different unit
// (or the same unit after expiry) issues a fresh request. Failures are never
// cached: an errored Search must re-hit upstream on the next call.
func TestSearchCachesWithinTTL(t *testing.T) {
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte(`{"success":true,"data":{"results":[
			{"url":"https://cdn.example/dl/4k/Show.S01E01.2160p.mkv","parsedFile":{"resolution":"2160p"}},
			{"url":"https://cdn.example/dl/hd/Show.S01E01.1080p.mkv","parsedFile":{"resolution":"1080p"}}
		]}}`))
	}))
	defer server.Close()

	c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")

	// Two tiers of the same episode: one upstream Search, both see all results.
	for i := 0; i < 2; i++ {
		streams, err := c.Search(context.Background(), "series", "tt1", 1, 1)
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		if len(streams) != 2 {
			t.Fatalf("search %d: streams = %d, want 2", i, len(streams))
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second tier served from cache)", got)
	}

	// A different unit is a cache miss and issues a fresh Search.
	if _, err := c.Search(context.Background(), "series", "tt1", 1, 2); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (new unit searches)", got)
	}

	// After the TTL lapses, the original unit re-searches rather than serving
	// a stale set — drive the injectable clock past expiry.
	c.now = func() time.Time { return time.Now().Add(2 * searchCacheTTL) }
	if _, err := c.Search(context.Background(), "series", "tt1", 1, 1); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Fatalf("upstream hits = %d, want 3 (expired entry re-searched)", got)
	}
}

// A classified failure (auth/rate-limit/transient) must never be cached as a
// success — the next call has to re-hit upstream so a recovered instance is seen.
func TestSearchDoesNotCacheFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&hits, 1)
				w.WriteHeader(status)
			}))
			defer server.Close()

			c := New(server.URL+"/stremio/uuid-1/blob/manifest.json", "pw")
			for i := 0; i < 2; i++ {
				if _, err := c.Search(context.Background(), "movie", "tt9", 0, 0); err == nil {
					t.Fatalf("search %d: want error for HTTP %d", i, status)
				}
			}
			if got := atomic.LoadInt64(&hits); got != 2 {
				t.Fatalf("upstream hits = %d, want 2 (failure not cached)", got)
			}
		})
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/silowebhook"
	"github.com/dreulavelle/wisp/internal/store"
)

// A file removed through the mount must both unpin and fire the Silo delete
// webhook — the same path the API delete takes.
func TestMountedDeleteUnpinsAndNotifiesSilo(t *testing.T) {
	var events []map[string]any
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		events = append(events, payload)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer silo.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	vpath := "shows/Demo (2026) [tvdb-1]/Season 01/Demo S01E01 [1080p].mkv"
	if err := st.Upsert(context.Background(), store.Pin{
		MediaType: "series", IMDbID: "tt1", Season: 1, Episode: 1,
		VirtualPath: vpath, ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	a := &app{store: st, log: slog.New(slog.DiscardHandler),
		webhook: silowebhook.New(silo.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))}

	if err := a.deleteMountedPin(context.Background(), vpath); err != nil {
		t.Fatalf("deleteMountedPin: %v", err)
	}
	if pin, _ := st.ByPath(context.Background(), vpath); pin != nil {
		t.Fatal("pin still present after mounted delete")
	}
	if len(events) != 1 || events[0]["eventType"] != "EpisodeFileDelete" {
		t.Fatalf("expected one EpisodeFileDelete webhook, got %#v", events)
	}
}

func TestProbeSizeUsesRangeTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=0-0" {
			t.Fatalf("probe request = %s range %q", r.Method, r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-0/987654321")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "x")
	}))
	defer server.Close()

	size, err := probeSize(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if size != 987654321 {
		t.Fatalf("size = %d", size)
	}
}

func TestProbeSizeAcceptsRangeIgnoredWithLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "123456")
	}))
	defer server.Close()

	size, err := probeSize(context.Background(), server.URL)
	if err != nil || size != 123456 {
		t.Fatalf("size = %d, err = %v", size, err)
	}
}

func TestProbeSizeRejectsResolverErrorPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, "not media")
	}))
	defer server.Close()

	if _, err := probeSize(context.Background(), server.URL); err == nil {
		t.Fatal("expected probe error")
	}
}

func TestSelectPlayableStreamFallsThrough(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/7654321")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer good.Close()

	streams := []aiostreams.Stream{
		{URL: bad.URL, Filename: "bad.mkv"},
		{URL: good.URL, Filename: "good.mp4", Resolution: "1080p"},
	}
	stream, size, err := selectPlayableStream(context.Background(), streams)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Filename != "good.mp4" || size != 7654321 {
		t.Fatalf("stream = %#v, size = %d", stream, size)
	}
}

// wispTestBackend fakes AIOStreams: /api/v1/search returns ranked results at two
// resolutions, and each result URL serves a ranged GET probe.
func wispTestBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		fmt.Fprintf(w, `{"success":true,"data":{"results":[
			{"url":%q,"filename":"Film.2160p.mkv","parsedFile":{"resolution":"2160p"}},
			{"url":%q,"filename":"Film.1080p.mkv","parsedFile":{"resolution":"1080p"}}
		]}}`, base+"/stream/2160", base+"/stream/1080")
	})
	probe := func(size string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "video/x-matroska")
			w.Header().Set("Content-Range", "bytes 0-0/"+size)
			w.WriteHeader(http.StatusPartialContent)
		}
	}
	mux.HandleFunc("/stream/2160", probe("42000000000"))
	mux.HandleFunc("/stream/1080", probe("9000000000"))
	return httptest.NewServer(mux)
}

func TestResolveEnforcesRequestedQuality(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()
	a := &app{aio: aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")}

	// Best-stream when no quality requested → top-ranked 2160p.
	_, _, _, res, err := a.resolve(context.Background(), "movie", "tt1", 0, 0, "")
	if err != nil || res != "2160p" {
		t.Fatalf("unconstrained resolve = %q (err %v), want 2160p (top rank)", res, err)
	}
	// Requesting 1080p must skip the higher-ranked 2160p result.
	url, _, _, res, err := a.resolve(context.Background(), "movie", "tt1", 0, 0, "1080p")
	if err != nil || res != "1080p" {
		t.Fatalf("1080p resolve = %q (err %v), want 1080p", res, err)
	}
	if !strings.HasSuffix(url, "/stream/1080") {
		t.Fatalf("selected url = %q, want the 1080p stream", url)
	}
	// A quality with no matching stream is a distinct, retriable condition.
	if _, _, _, _, err := a.resolve(context.Background(), "movie", "tt1", 0, 0, "720p"); !errors.Is(err, errNoQualityMatch) {
		t.Fatalf("720p resolve err = %v, want errNoQualityMatch", err)
	}
}

func TestWriteAddErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"no results", errNoResults, http.StatusBadGateway, "no_streams"},
		{"no playable", errNoPlayable, http.StatusBadGateway, "no_streams"},
		{"no quality match", errNoQualityMatch, http.StatusBadGateway, "no_quality_match"},
		{"auth", &aiostreams.SearchError{Kind: aiostreams.KindAuth, Status: 401}, http.StatusInternalServerError, "aiostreams_auth"},
		{"rate limited", &aiostreams.SearchError{Kind: aiostreams.KindRateLimited, Status: 429}, http.StatusTooManyRequests, "rate_limited"},
		{"transient", &aiostreams.SearchError{Kind: aiostreams.KindTransient, Status: 502}, http.StatusServiceUnavailable, "upstream_unavailable"},
		// An unclassified error (bad URL, decode failure, success:false) must NOT
		// masquerade as a monitorable 502 no_streams.
		{"unclassified", errors.New("invalid AIOStreams URL"), http.StatusServiceUnavailable, "upstream_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAddError(rec, slog.New(slog.DiscardHandler), addRequest{MediaType: "movie", IMDbID: "tt1"}, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["error"] != tc.wantCode {
				t.Fatalf("error code = %v, want %q", body["error"], tc.wantCode)
			}
		})
	}
}

func TestQualityLabel(t *testing.T) {
	cases := []struct {
		resolution, filename, want, expect string
	}{
		{"2160p", "", "", "2160p"},
		{"4K", "", "", "2160p"}, // canonicalized so delete/filter agree
		{"", "Film.1080p.mkv", "", "1080p"},
		{"", "", "2160p", "2160p"}, // requested quality when nothing parsed
		{"540p", "", "", "540p"},   // uncommon resolution kept verbatim
		{"", "", "", "1080p"},      // last-resort default
	}
	for _, tc := range cases {
		if got := qualityLabel(tc.resolution, tc.filename, tc.want); got != tc.expect {
			t.Fatalf("qualityLabel(%q,%q,%q) = %q, want %q", tc.resolution, tc.filename, tc.want, got, tc.expect)
		}
	}
}

// A non-empty but unrecognized quality must be rejected, not silently widened
// into a delete-all.
func TestDeletePinRejectsUnknownQuality(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Upsert(context.Background(), store.Pin{IMDbID: "tt5", MediaType: "movie", Quality: "1080p", VirtualPath: "movies/X - [1080p].mkv"})

	a := &app{store: st, log: slog.New(slog.DiscardHandler),
		webhook: silowebhook.New("", "", slog.New(slog.DiscardHandler))}
	rec := httptest.NewRecorder()
	body := `{"imdb_id":"tt5","quality":"1o80p"}`
	a.handleDeletePin(rec, httptest.NewRequest(http.MethodDelete, "/api/pins", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unrecognized quality", rec.Code)
	}
	if n, _ := st.Count(context.Background()); n != 1 {
		t.Fatalf("pin count = %d, want 1 (nothing deleted)", n)
	}
}

// Adding two qualities of the same episode must yield two coexisting pins — the
// rename/supersede cleanup must not treat a different tier as a rename.
func TestHandleAddQualityPinsCoexist(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &app{
		store: st, log: slog.New(slog.DiscardHandler),
		aio:     aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw"),
		webhook: silowebhook.New("", "", slog.New(slog.DiscardHandler)),
	}

	add := func(quality string) int {
		body, _ := json.Marshal(addRequest{
			MediaType: "series", IMDbID: "tt7", Title: "Demo", Year: 2026,
			Season: 1, Episode: 1, Quality: quality, TMDbID: "555", // TMDbID skips Cinemeta
		})
		rec := httptest.NewRecorder()
		a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(string(body))))
		return rec.Code
	}

	if code := add("1080p"); code != http.StatusOK {
		t.Fatalf("add 1080p status = %d", code)
	}
	if code := add("2160p"); code != http.StatusOK {
		t.Fatalf("add 2160p status = %d", code)
	}

	n, _ := st.Count(context.Background())
	if n != 2 {
		t.Fatalf("pin count = %d, want 2 (1080p and 2160p coexist)", n)
	}
	for _, q := range []string{"1080p", "2160p"} {
		want := "shows/Demo (2026) [tmdb-555]/Season 01/Demo (2026) - S01E01 - [" + q + "].mkv"
		if p, _ := st.ByPath(context.Background(), want); p == nil {
			t.Fatalf("missing %s pin at %q", q, want)
		}
	}
}

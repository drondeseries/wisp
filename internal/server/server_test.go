package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/store"
)

// getReq builds a GET request for a virtual path, escaping spaces and other
// characters the way rclone/clients would on the wire.
func getReq(vpath string) *http.Request {
	target := (&url.URL{Path: "/" + strings.TrimPrefix(vpath, "/")}).String()
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func pinEpisode(t *testing.T, st *store.Store, sourceURL string, size int64) store.Pin {
	t.Helper()
	p := store.Pin{
		MediaType: "series", IMDbID: "tt1", Season: 1, Episode: 4, Title: "Show", Year: 2026,
		Quality: "1080p", VirtualPath: "shows/Show (2026)/Season 01/Show (2026) - S01E04 - [1080p].mkv",
		SourceURL: sourceURL, Size: size, ResolvedAt: time.Now(),
	}
	if err := st.Upsert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestServeFileRangeProxy proves a Range request is forwarded to the upstream
// and its 206 body + headers are mirrored back to the client.
func TestServeFileRangeProxy(t *testing.T) {
	const body = "0123456789abcdef"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=4-7" {
			t.Fatalf("range forwarded = %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 4-7/16")
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, body[4:8])
	}))
	defer upstream.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, upstream.URL, int64(len(body)))
	srv := New(st, nil, discard())

	req := getReq(pin.VirtualPath)
	req.Header.Set("Range", "bytes=4-7")
	rec := httptest.NewRecorder()
	srv.FileHandler(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "4567" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Range") != "bytes 4-7/16" {
		t.Fatalf("content-range = %q", rec.Header().Get("Content-Range"))
	}
}

// TestServeFileReResolvesOnDeadLink proves the self-heal path: a dead upstream
// (HTTP 404) triggers ReResolve, and the retried request serves from the new
// upstream. This is the durability guarantee.
func TestServeFileReResolvesOnDeadLink(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer dead.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "healed")
	}))
	defer live.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, dead.URL, 6)

	var reresolveCalls int
	reresolve := func(ctx context.Context, p *store.Pin) error {
		reresolveCalls++
		p.SourceURL = live.URL
		return st.UpdateResolution(ctx, p.VirtualPath, live.URL, 6)
	}
	srv := New(st, reresolve, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq(pin.VirtualPath))

	if reresolveCalls != 1 {
		t.Fatalf("reresolve calls = %d, want 1", reresolveCalls)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "healed" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestServeFileGivesUpAfterReResolve proves that if the upstream is still dead
// after a re-resolve, the client gets a clean 502 rather than a hang.
func TestServeFileGivesUpAfterReResolve(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer dead.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, dead.URL, 6)
	reresolve := func(ctx context.Context, p *store.Pin) error { return nil } // still points at dead
	srv := New(st, reresolve, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq(pin.VirtualPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestHeadReportsSizeWithoutUpstream proves a HEAD returns the pinned size with
// no upstream call, so scanners can stat the file cheaply.
func TestHeadReportsSizeWithoutUpstream(t *testing.T) {
	st := newTestStore(t)
	pin := pinEpisode(t, st, "http://never.called", 1471496964)
	srv := New(st, nil, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, httptest.NewRequest(http.MethodHead, (&url.URL{Path: "/" + pin.VirtualPath}).String(), nil))
	if got := rec.Header().Get("Content-Length"); got != "1471496964" {
		t.Fatalf("content-length = %q", got)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("accept-ranges = %q", rec.Header().Get("Accept-Ranges"))
	}
}

// TestDirectoryListing proves the tree is synthesized from pinned paths, so
// rclone's :http: backend can walk it.
func TestDirectoryListing(t *testing.T) {
	st := newTestStore(t)
	pinEpisode(t, st, "http://x", 1)
	srv := New(st, nil, discard())

	root := httptest.NewRecorder()
	srv.FileHandler(root, getReq(""))
	if !strings.Contains(root.Body.String(), `href="shows/"`) {
		t.Fatalf("root listing = %q", root.Body.String())
	}

	season := httptest.NewRecorder()
	srv.FileHandler(season, getReq("shows/Show (2026)/Season 01/"))
	if !strings.Contains(season.Body.String(), ".mkv") {
		t.Fatalf("season listing = %q", season.Body.String())
	}

	missing := httptest.NewRecorder()
	srv.FileHandler(missing, getReq("shows/Nope/"))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing dir status = %d", missing.Code)
	}
}

// TestReResolveNotAttemptedMidStream proves that once bytes are committed to
// the client, a mid-stream failure does not trigger a re-resolve (which would
// corrupt the response). Re-resolve is only safe before the first byte.
func TestReResolveNotAttemptedMidStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
		// Then the handler returns; body is short but headers already sent 200.
	}))
	defer upstream.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, upstream.URL, 100)
	var reresolveCalls int
	srv := New(st, func(context.Context, *store.Pin) error { reresolveCalls++; return nil }, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq(pin.VirtualPath))
	if reresolveCalls != 0 {
		t.Fatalf("re-resolve attempted after commit: %d", reresolveCalls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestConcurrentRangeReads hammers one file with many simultaneous range
// requests (rclone prefetch + multiple viewers) to surface data races and
// prove the proxy stays correct under load. Run with -race.
func TestConcurrentRangeReads(t *testing.T) {
	const size = 1 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mkv", time.Unix(0, 0), strings.NewReader(string(payload)))
	}))
	defer upstream.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, upstream.URL, size)
	srv := New(st, nil, discard())

	const workers = 32
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			start := (w * 7919) % (size - 4096)
			req := getReq(pin.VirtualPath)
			req.Header.Set("Range", fmtRange(start, start+4095))
			rec := httptest.NewRecorder()
			srv.FileHandler(rec, req)
			if rec.Code != http.StatusPartialContent {
				errs <- fmtErr("worker %d status %d", w, rec.Code)
				return
			}
			got := rec.Body.Bytes()
			if len(got) != 4096 || got[0] != payload[start] {
				errs <- fmtErr("worker %d wrong bytes", w)
				return
			}
			errs <- nil
		}(w)
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func fmtRange(a, b int) string        { return fmt.Sprintf("bytes=%d-%d", a, b) }
func fmtErr(f string, a ...any) error { return fmt.Errorf(f, a...) }

// TestPathTraversalCannotEscape proves the server never touches the filesystem:
// odd or traversal-style paths resolve only against pinned bbolt keys, so they
// can only ever 404, never read a real file.
func TestPathTraversalCannotEscape(t *testing.T) {
	st := newTestStore(t)
	pinEpisode(t, st, "http://x", 1)
	srv := New(st, nil, discard())

	for _, target := range []string{
		"/../../../../etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
		"/shows/../../../etc/passwd",
		"/./shows/./",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		srv.FileHandler(rec, req)
		body := rec.Body.String()
		if strings.Contains(body, "root:") || strings.Contains(body, "/bin/") {
			t.Fatalf("target %q leaked filesystem content: %q", target, body)
		}
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusOK {
			t.Fatalf("target %q status = %d", target, rec.Code)
		}
	}
}

// TestUnknownFile404s proves a request for a path with no pin is a clean 404.
func TestUnknownFile404s(t *testing.T) {
	st := newTestStore(t)
	srv := New(st, nil, discard())
	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq("shows/Ghost (2099)/Season 01/nope.mkv"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestMethodNotAllowed proves non-GET/HEAD methods are rejected cleanly.
func TestMethodNotAllowed(t *testing.T) {
	st := newTestStore(t)
	srv := New(st, nil, discard())
	rec := httptest.NewRecorder()
	srv.FileHandler(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestCDNLinkCacheSkipsRedirect proves the latency fix: the first read follows
// the permalink's redirect to the CDN and caches it; subsequent reads hit the
// CDN directly, so the permalink is touched only once.
func TestCDNLinkCacheSkipsRedirect(t *testing.T) {
	var permalinkHits, cdnHits int
	var mu sync.Mutex
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cdnHits++
		mu.Unlock()
		w.Header().Set("Content-Range", "bytes 0-3/16")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "data")
	}))
	defer cdn.Close()
	permalink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		permalinkHits++
		mu.Unlock()
		http.Redirect(w, r, cdn.URL+"/file.mkv", http.StatusFound)
	}))
	defer permalink.Close()

	st := newTestStore(t)
	pin := pinEpisode(t, st, permalink.URL, 16)
	srv := New(st, nil, discard())

	for i := 0; i < 4; i++ {
		req := getReq(pin.VirtualPath)
		req.Header.Set("Range", "bytes=0-3")
		rec := httptest.NewRecorder()
		srv.FileHandler(rec, req)
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "data" {
			t.Fatalf("read %d: code=%d body=%q", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if permalinkHits != 1 {
		t.Fatalf("permalink hit %d times, want 1 (cache should skip it after first)", permalinkHits)
	}
	if cdnHits != 4 {
		t.Fatalf("cdn hit %d times, want 4", cdnHits)
	}
}

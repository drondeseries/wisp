package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/seerr"
	"github.com/dreulavelle/wisp/internal/store"
)

// offlineApp builds an app whose metadata heuristic points at a stub Cinemeta
// that reports no genres, so intake never touches the real network.
func offlineApp(t *testing.T) *app {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{}}`))
	}))
	t.Cleanup(stub.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{
		store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil, metadata.WithBaseURLs(stub.URL, stub.URL, stub.URL)),
		seerr:   seerr.New("", ""),
		webhook: notify.New(notify.Options{}, log),
	}
	a.mon = monitor.New(st, a.meta, a, time.Hour, log)
	return a
}

// A request-shaped /api/add registers a monitor (async), returns 202, maps the
// qualities array, records the request_ref, and is idempotent per title.
func TestHandleAddRequestShapedIntake(t *testing.T) {
	a := offlineApp(t)
	body := `{"media_type":"series","imdb_id":"tt7","title":"Show","year":2026,
		"qualities":[{"id":"1080p"},{"id":"4k","is4k":true}],"request_ref":"silo-42"}`

	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 {
		t.Fatalf("monitors = %d, want 1", len(items))
	}
	it := items[0]
	if it.RequestRef != "silo-42" {
		t.Fatalf("request_ref = %q", it.RequestRef)
	}
	if len(it.Qualities) != 2 || it.Qualities[0] != "1080p" || it.Qualities[1] != "2160p" {
		t.Fatalf("qualities = %v, want [1080p 2160p]", it.Qualities)
	}
	if it.Category != "" { // no explicit flag + no pins → deferred to the scheduler
		t.Fatalf("category = %q, want empty (deferred, no synchronous heuristic)", it.Category)
	}
	if n, _ := a.store.Count(context.Background()); n != 0 {
		t.Fatalf("request-shaped add pinned synchronously: %d pins", n)
	}

	// Idempotent: re-posting the same title extends, not duplicates.
	rec = httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second add status = %d", rec.Code)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 1 {
		t.Fatalf("monitors after re-add = %d, want 1 (idempotent)", len(items))
	}
}

// The explicit is_anime flag routes a request-shaped add into the anime root.
func TestHandleAddRequestShapedAnime(t *testing.T) {
	a := offlineApp(t)
	body := `{"media_type":"movie","tmdb_id":"603","imdb_id":"tt6","title":"Akira","is_anime":true}`
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 || items[0].Category != "anime_movies" {
		t.Fatalf("monitored = %#v, want anime_movies", items)
	}
}

// A legacy direct-pin payload (imdb + season/episode/quality) still resolves and
// pins synchronously, under the non-anime root — byte-identical to before.
func TestHandleAddLegacyStillPinsSynchronously(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	a := offlineApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")

	body := `{"media_type":"series","imdb_id":"tt7","title":"Demo","year":2026,
		"season":1,"episode":1,"quality":"1080p","tmdb_id":"555"}`
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy add status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	vp, _ := resp["virtual_path"].(string)
	want := "shows/Demo (2026) [tmdb-555]/Season 01/Demo (2026) - S01E01 - [1080p].mkv"
	if vp != want {
		t.Fatalf("virtual_path = %q, want %q", vp, want)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 0 {
		t.Fatalf("legacy add created a monitor: %#v", items)
	}
}

// computeRequestStatus covers the mapping table without HTTP or network.
func TestComputeRequestStatus(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	servable := store.Pin{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/x", SourceURL: "http://a", Size: 1}
	unservable := store.Pin{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/x", SourceURL: "", Size: 0}

	cases := []struct {
		name       string
		mon        *store.Monitored
		pins       []store.Pin
		mediaType  string
		wantState  string
		wantDetail string
	}{
		{
			name:      "movie unreleased is queued, never failed",
			mon:       &store.Monitored{MediaType: "movie", DueAt: future, DueReason: store.DueReasonRelease},
			mediaType: "movie", wantState: statusQueued, wantDetail: "awaiting home-media release",
		},
		{
			name:      "movie released, no stream yet, retry window is queued",
			mon:       &store.Monitored{MediaType: "movie", DueAt: now.Add(-time.Hour), DueReason: store.DueReasonRetry, LastChecked: now},
			mediaType: "movie", wantState: statusQueued, wantDetail: "resolving stream",
		},
		{
			name:      "movie pinned is completed",
			mon:       &store.Monitored{MediaType: "movie", LastChecked: now},
			pins:      []store.Pin{servable},
			mediaType: "movie", wantState: statusCompleted,
		},
		{
			name:      "movie with only an unservable pin stays queued",
			mon:       &store.Monitored{MediaType: "movie", LastChecked: now},
			pins:      []store.Pin{unservable},
			mediaType: "movie", wantState: statusQueued,
		},
		{
			name: "movie multi-tier: only 1080p pinned is queued",
			mon:  &store.Monitored{MediaType: "movie", LastChecked: now, Qualities: []string{"1080p", "2160p"}},
			pins: []store.Pin{
				{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/a", SourceURL: "http://a", Size: 1},
			},
			mediaType: "movie", wantState: statusQueued,
		},
		{
			name: "movie multi-tier: both tiers pinned is completed",
			mon:  &store.Monitored{MediaType: "movie", LastChecked: now, Qualities: []string{"1080p", "2160p"}},
			pins: []store.Pin{
				{MediaType: "movie", Quality: "1080p", VirtualPath: "movies/a", SourceURL: "http://a", Size: 1},
				{MediaType: "movie", Quality: "2160p", VirtualPath: "movies/b", SourceURL: "http://a", Size: 1},
			},
			mediaType: "movie", wantState: statusCompleted,
		},
		{
			name:      "series unaired (checked, no pins) is queued",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 0, DueAt: future, DueReason: store.DueReasonAirstamp},
			mediaType: "series", wantState: statusQueued, wantDetail: "awaiting next episode airing",
		},
		{
			name:      "series all aired episodes pinned is completed",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 0},
			pins:      []store.Pin{{MediaType: "series", Quality: "1080p", VirtualPath: "shows/x", SourceURL: "http://a", Size: 1}},
			mediaType: "series", wantState: statusCompleted,
		},
		{
			name:      "series still catching up is queued",
			mon:       &store.Monitored{MediaType: "series", LastChecked: now, PendingAired: 2},
			pins:      []store.Pin{{MediaType: "series", Quality: "1080p", VirtualPath: "shows/x", SourceURL: "http://a", Size: 1}},
			mediaType: "series", wantState: statusQueued,
		},
		{
			name:      "permanent give-up is failed",
			mon:       &store.Monitored{MediaType: "series", Failed: true, LastError: "unresolvable identity"},
			mediaType: "series", wantState: statusFailed, wantDetail: "unresolvable identity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRequestStatus(tc.mon, tc.pins, tc.mediaType, now)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// The HTTP endpoint round-trips: 404 for an untracked title, 200 + mapped state
// for a tracked one, matched by tmdb_id.
func TestHandleRequestStatusHTTP(t *testing.T) {
	a := offlineApp(t)
	ctx := context.Background()

	// Untracked → 404.
	rec := httptest.NewRecorder()
	a.handleRequestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/requests/status?media_type=movie&tmdb_id=999", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("untracked status = %d, want 404", rec.Code)
	}

	// A monitored, completed movie matched by tmdb_id.
	_ = a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt6", MediaType: "movie", IMDbID: "tt6", TMDbID: "603",
		Category: "movies", LastChecked: time.Now(), RequestRef: "silo-7",
	})
	_ = a.store.Upsert(ctx, store.Pin{
		MediaType: "movie", IMDbID: "tt6", TMDbID: "603", Quality: "2160p",
		VirtualPath: "movies/Akira (1988)/a.mkv", SourceURL: "http://a", Size: 10,
	})

	rec = httptest.NewRecorder()
	a.handleRequestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/requests/status?media_type=movie&tmdb_id=603", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp requestStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != statusCompleted {
		t.Fatalf("state = %q, want completed", resp.State)
	}
	if len(resp.PinnedQualities) != 1 || resp.PinnedQualities[0] != "2160p" {
		t.Fatalf("pinned_qualities = %v", resp.PinnedQualities)
	}
	if resp.RequestRef != "silo-7" {
		t.Fatalf("request_ref = %q", resp.RequestRef)
	}
}

// isRequestShaped: the presence of the qualities field (even empty), request_ref,
// is_anime, or a tmdb-only identity marks a request; a legacy imdb + season/
// episode/quality payload does not.
func TestIsRequestShaped(t *testing.T) {
	empty := []qualitySpec{}
	oneTier := []qualitySpec{{ID: "1080p"}}
	cases := []struct {
		name string
		req  addRequest
		want bool
	}{
		{"legacy imdb direct pin", addRequest{IMDbID: "tt1", Season: 1, Episode: 2, Quality: "1080p"}, false},
		{"legacy imdb movie", addRequest{IMDbID: "tt1"}, false},
		{"empty qualities array present", addRequest{IMDbID: "tt1", Qualities: &empty}, true},
		{"non-empty qualities", addRequest{IMDbID: "tt1", Qualities: &oneTier}, true},
		{"request_ref", addRequest{IMDbID: "tt1", RequestRef: "silo-1"}, true},
		{"is_anime", addRequest{IMDbID: "tt1", IsAnime: boolPtr(false)}, true},
		{"tmdb only", addRequest{TMDbID: "603"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.isRequestShaped(); got != tc.want {
				t.Fatalf("isRequestShaped() = %v, want %v", got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// A present-but-empty qualities array routes to async intake, NOT the legacy
// sync path — which for a series with no season/episode would pin a bogus
// S00E00 file. The aio backend is live, so a wrong legacy route WOULD pin.
func TestHandleAddEmptyQualitiesRoutesToIntake(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()
	a := offlineApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")

	body := `{"media_type":"series","imdb_id":"tt9","title":"Show","qualities":[]}`
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (empty qualities = request-shaped)", rec.Code)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 1 {
		t.Fatalf("monitors = %d, want 1", len(items))
	}
	if n, _ := a.store.Count(context.Background()); n != 0 {
		t.Fatalf("pinned synchronously: %d pins (S00E00 regression)", n)
	}
}

// A tmdb-only status query finds a legacy/direct pin that is imdb-keyed but
// carries a persisted TMDbID (fix: match on Pin.TMDbID, not only the
// "tmdb:<id>" search-id convention).
func TestHandleRequestStatusMatchesPinByTMDbID(t *testing.T) {
	a := offlineApp(t)
	ctx := context.Background()
	// Legacy direct pin: imdb-keyed, TMDbID persisted, and NO monitor.
	_ = a.store.Upsert(ctx, store.Pin{
		MediaType: "movie", IMDbID: "tt6", TMDbID: "603", Quality: "1080p",
		VirtualPath: "movies/Akira (1988)/a.mkv", SourceURL: "http://a", Size: 10,
	})

	rec := httptest.NewRecorder()
	a.handleRequestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/requests/status?media_type=movie&tmdb_id=603", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (pin found by tmdb id, not 404)", rec.Code)
	}
	var resp requestStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != statusCompleted {
		t.Fatalf("state = %q, want completed", resp.State)
	}
	if len(resp.PinnedQualities) != 1 || resp.PinnedQualities[0] != "1080p" {
		t.Fatalf("pinned_qualities = %v", resp.PinnedQualities)
	}
}

// The intake path must never block on metadata: /api/add returns 202 fast even
// when the Cinemeta backend is pathologically slow (the heuristic is deferred to
// the scheduler).
func TestIntakeDoesNotBlockOnMetadata(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second) // far longer than any acceptable add latency
		w.Write([]byte(`{"meta":{"genres":["Animation"],"country":"Japan"}}`))
	}))
	defer slow.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.DiscardHandler)
	a := &app{
		store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil, metadata.WithBaseURLs(slow.URL, slow.URL, slow.URL)),
		seerr:   seerr.New("", ""),
		webhook: notify.New(notify.Options{}, log),
	}
	a.mon = monitor.New(st, a.meta, a, time.Hour, log)

	// imdb present (no tmdb→imdb lookup), is_anime omitted → the only metadata
	// call would be the heuristic, which must be deferred.
	body := `{"media_type":"series","imdb_id":"tt7","title":"Show","qualities":[{"id":"1080p"}]}`
	start := time.Now()
	rec := httptest.NewRecorder()
	a.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body)))
	elapsed := time.Since(start)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("intake blocked on metadata: took %v (heuristic must be deferred)", elapsed)
	}
}

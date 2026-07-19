package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/store"
)

func scheduleTestApp(t *testing.T) *app {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil),
		webhook: notify.New(notify.Options{}, log)}
	a.mon = monitor.New(st, a.meta, a, 2*time.Hour, 4, log)
	return a
}

func TestHandleScheduleReportsState(t *testing.T) {
	a := scheduleTestApp(t)
	ctx := context.Background()
	now := time.Now()

	// A movie whose DueAt is a real release date in the future → "waiting".
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt1", MediaType: "movie", IMDbID: "tt1", Title: "Future Film",
		Qualities: []string{"1080p"}, DueAt: now.Add(48 * time.Hour),
		DueReason: store.DueReasonRelease, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A released movie with no stream yet: future DueAt but only a retry ceiling.
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt4", MediaType: "movie", IMDbID: "tt4", Title: "Retrying Film",
		Qualities: []string{"1080p"}, DueAt: now.Add(2 * time.Hour),
		DueReason: store.DueReasonRetry, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// A completed movie with a pin and a zero DueAt (processMovie returns zero).
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "movie:tt2", MediaType: "movie", IMDbID: "tt2", Title: "Done Film",
		Qualities: []string{"1080p"}, Enabled: true, Completed: true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.store.Upsert(ctx, store.Pin{IMDbID: "tt2", MediaType: "movie", Quality: "1080p",
		VirtualPath: "movies/Done Film [1080p].mkv", ResolvedAt: now})
	// A due series (past DueAt), two tiers requested, one pinned → 1 pending.
	if err := a.store.PutMonitored(ctx, store.Monitored{
		Key: "series:tt3", MediaType: "series", IMDbID: "tt3", Title: "Show",
		Qualities: []string{"1080p", "2160p"}, DueAt: now.Add(-time.Minute), Enabled: true,
		LastChecked: now.Add(-2 * time.Minute), LastError: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	_ = a.store.Upsert(ctx, store.Pin{IMDbID: "tt3", MediaType: "series", Season: 1, Episode: 1,
		Quality: "1080p", VirtualPath: "shows/Show/Season 01/Show S01E01 [1080p].mkv", ResolvedAt: now})

	rec := httptest.NewRecorder()
	a.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var resp scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if resp.IntervalSeconds != 7200 {
		t.Fatalf("interval_seconds = %d, want 7200", resp.IntervalSeconds)
	}
	if len(resp.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(resp.Items))
	}
	byKey := map[string]scheduleItem{}
	for _, it := range resp.Items {
		byKey[it.Key] = it
	}

	// Release date → waiting + next_release + next_check.
	future := byKey["movie:tt1"]
	if future.State != "waiting" {
		t.Fatalf("tt1 state = %q, want waiting", future.State)
	}
	if future.NextRelease == nil {
		t.Fatal("tt1 (release date) should carry next_release")
	}
	if future.NextCheck == nil {
		t.Fatal("tt1 should carry next_check")
	}
	if future.PendingTargets != 1 {
		t.Fatalf("tt1 pending = %d, want 1", future.PendingTargets)
	}

	// Retry ceiling → retrying, and NO next_release even though DueAt is future.
	retry := byKey["movie:tt4"]
	if retry.State != "retrying" {
		t.Fatalf("tt4 state = %q, want retrying", retry.State)
	}
	if retry.NextRelease != nil {
		t.Fatal("tt4 (retry ceiling) must not carry next_release")
	}
	if retry.NextCheck == nil {
		t.Fatal("tt4 should still carry next_check (its retry time)")
	}

	// Completed movie with zero DueAt → next_check omitted (no epoch-underflow).
	done := byKey["movie:tt2"]
	if done.State != "completed" || done.PendingTargets != 0 {
		t.Fatalf("tt2 = %+v, want completed/0", done)
	}
	if done.NextCheck != nil {
		t.Fatalf("tt2 (zero DueAt) must omit next_check, got %d", *done.NextCheck)
	}
	if done.Pinned != 1 {
		t.Fatalf("tt2 pinned = %d, want 1", done.Pinned)
	}

	series := byKey["series:tt3"]
	if series.State != "pending" {
		t.Fatalf("tt3 state = %q, want pending", series.State)
	}
	if series.LastError != "boom" {
		t.Fatalf("tt3 last_error = %q", series.LastError)
	}
	if series.PendingTargets != 1 { // 2160p tier has nothing pinned
		t.Fatalf("tt3 pending = %d, want 1", series.PendingTargets)
	}
	if series.NextRelease != nil {
		t.Fatal("a due (past) item must not carry next_release")
	}
}

// next_wake reflects the scheduler's real armed deadline, not now+interval.
func TestScheduleNextWakeUsesArmedDeadline(t *testing.T) {
	a := scheduleTestApp(t)
	ctx := context.Background()

	// No timer armed yet → fall back to now+interval.
	view, err := a.buildSchedule(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fallback := time.Now().Add(a.mon.Interval()).Unix()
	if diff := view.NextWake - fallback; diff < -2 || diff > 2 {
		t.Fatalf("pre-arm next_wake = %d, want ~%d", view.NextWake, fallback)
	}

	// Arm a deadline by running the scheduler until it sleeps, then confirm the
	// reported next_wake matches that deadline (not a fresh now+interval).
	runCtx, cancel := context.WithCancel(ctx)
	go a.mon.Run(runCtx)
	defer cancel()
	deadline := waitForArmedWake(t, a)

	view, err = a.buildSchedule(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.NextWake != deadline.Unix() {
		t.Fatalf("next_wake = %d, want armed deadline %d", view.NextWake, deadline.Unix())
	}
}

func waitForArmedWake(t *testing.T, a *app) time.Time {
	t.Helper()
	for i := 0; i < 200; i++ {
		if w := a.mon.NextWake(); !w.IsZero() {
			return w
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("scheduler never armed a wake deadline")
	return time.Time{}
}

func TestHandleScheduleEmpty(t *testing.T) {
	a := scheduleTestApp(t)
	rec := httptest.NewRecorder()
	a.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(resp.Items))
	}
	if resp.IntervalSeconds != 7200 {
		t.Fatalf("interval = %d", resp.IntervalSeconds)
	}
}

func TestPausedItemState(t *testing.T) {
	a := scheduleTestApp(t)
	_ = a.store.PutMonitored(context.Background(), store.Monitored{
		Key: "movie:tt9", MediaType: "movie", IMDbID: "tt9", Enabled: false,
		DueAt: time.Now().Add(-time.Hour),
	})
	view, err := a.buildSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].State != "paused" {
		t.Fatalf("items = %+v, want one paused", view.Items)
	}
}

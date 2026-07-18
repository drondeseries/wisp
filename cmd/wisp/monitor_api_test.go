package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/seerr"
	"github.com/dreulavelle/wisp/internal/store"
)

func testApp(t *testing.T) *app {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wisp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.DiscardHandler)
	a := &app{
		store: st, log: log, startedAt: time.Now(),
		meta:    metadata.New("", nil),
		seerr:   seerr.New("", ""), // unconfigured → Enrich no-ops
		webhook: notify.New(notify.Options{}, log),
	}
	a.mon = monitor.New(st, a.meta, a, time.Hour, log) // Run not started → Intake only records
	return a
}

func TestHandleSeerrWebhookCreatesMonitor(t *testing.T) {
	a := testApp(t)
	body := `{"notification_type":"MEDIA_AUTO_APPROVED","subject":"The Villager of Level 999 (2026)",
		"media":{"media_type":"tv","tmdbId":"38262097","imdbId":"tt38262097"},
		"request":{"request_id":"3","is4k":false},"extra":[{"name":"Requested Seasons","value":"1"}]}`
	rec := httptest.NewRecorder()
	a.handleSeerrWebhook(rec, httptest.NewRequest(http.MethodPost, "/api/seerr", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 || items[0].MediaType != "series" || items[0].IMDbID != "tt38262097" {
		t.Fatalf("monitored = %#v", items)
	}
	if len(items[0].Qualities) != 1 || items[0].Qualities[0] != "1080p" {
		t.Fatalf("qualities = %v, want [1080p]", items[0].Qualities)
	}
	if len(items[0].Seasons) != 1 || items[0].Seasons[0] != 1 {
		t.Fatalf("seasons = %v, want [1]", items[0].Seasons)
	}
}

func TestHandleSeerrWebhookIgnoresTestPing(t *testing.T) {
	a := testApp(t)
	rec := httptest.NewRecorder()
	a.handleSeerrWebhook(rec, httptest.NewRequest(http.MethodPost, "/api/seerr", strings.NewReader(`{"notification_type":"TEST_NOTIFICATION"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ignored)", rec.Code)
	}
	if items, _ := a.store.ListMonitored(context.Background()); len(items) != 0 {
		t.Fatalf("test ping created monitors: %#v", items)
	}
}

func TestMonitorCRUD(t *testing.T) {
	a := testApp(t)
	// Create
	rec := httptest.NewRecorder()
	a.handleCreateMonitor(rec, httptest.NewRequest(http.MethodPost, "/api/monitors",
		strings.NewReader(`{"media_type":"movie","imdb_id":"tt1375666","title":"Inception","year":2010,"qualities":["4k","1080p"]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	items, _ := a.store.ListMonitored(context.Background())
	if len(items) != 1 {
		t.Fatalf("monitors after create = %d", len(items))
	}
	if got := items[0].Qualities; len(got) != 2 || got[0] != "2160p" || got[1] != "1080p" {
		t.Fatalf("qualities normalized = %v", got) // "4k" → "2160p"
	}
	key := items[0].Key

	// List
	rec = httptest.NewRecorder()
	a.handleListMonitors(rec, httptest.NewRequest(http.MethodGet, "/api/monitors", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Inception") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// Delete
	rec = httptest.NewRecorder()
	a.handleDeleteMonitor(rec, httptest.NewRequest(http.MethodDelete, "/api/monitors?key="+key, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if n, _ := a.store.CountMonitored(context.Background()); n != 0 {
		t.Fatalf("monitors after delete = %d", n)
	}
}

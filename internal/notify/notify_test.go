package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestNewResolvesTargets(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	// Alias-only: WISP_SILO_WEBHOOK_URL still configures the arr target.
	m := New(Options{SiloWebhookURL: "http://silo/hook"}, log)
	if got := m.Targets(); !slices.Equal(got, []string{"arr-webhook"}) {
		t.Fatalf("alias-only targets = %v", got)
	}

	// Canonical wins over alias when both are set.
	m = New(Options{ArrWebhookURL: "http://canon/hook", SiloWebhookURL: "http://silo/hook"}, log)
	arr := m.targets[0].(*arrTarget)
	if arr.url != "http://canon/hook" {
		t.Fatalf("canonical did not win: %q", arr.url)
	}

	// All targets configured.
	m = New(Options{
		ArrWebhookURL: "http://arr", JellyfinURL: "http://jf", EmbyURL: "http://emby", PlexURL: "http://plex",
	}, log)
	if got := m.Targets(); !slices.Equal(got, []string{"arr-webhook", "jellyfin", "emby", "plex"}) {
		t.Fatalf("all targets = %v", got)
	}

	// No targets: safe no-op notifier.
	m = New(Options{}, log)
	if len(m.Targets()) != 0 {
		t.Fatalf("expected no targets, got %v", m.Targets())
	}
	m.Import(context.Background(), "movie", "movies/x.mkv") // must not panic
}

// The Multi fans out to every target and detaches from the caller's context, so
// a cancelled request context does not abort delivery.
func TestMultiFanoutDetachesContext(t *testing.T) {
	got := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- "hit"
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	m := New(Options{ArrWebhookURL: server.URL, MountPath: "/mnt/wisp"}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	m.Import(ctx, "movie", "movies/New/movie.mkv")
	cancel() // cancel immediately; delivery must still complete

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook was not delivered after context cancel")
	}
}

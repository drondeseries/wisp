package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type capturedRequest struct {
	method string
	path   string
	token  string
	body   map[string]any
}

func mediaBrowserServer(t *testing.T, reqs *[]capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*reqs = append(*reqs, capturedRequest{
			method: r.Method, path: r.URL.Path,
			token: r.Header.Get("X-Emby-Token"), body: body,
		})
		w.WriteHeader(http.StatusNoContent)
	}))
}

func updateTypes(t *testing.T, req capturedRequest) []string {
	t.Helper()
	updates, ok := req.body["Updates"].([]any)
	if !ok {
		t.Fatalf("missing Updates: %#v", req.body)
	}
	var out []string
	for _, u := range updates {
		out = append(out, u.(map[string]any)["updateType"].(string))
	}
	return out
}

func firstUpdatePath(t *testing.T, req capturedRequest) string {
	t.Helper()
	updates := req.body["Updates"].([]any)
	return updates[0].(map[string]any)["path"].(string)
}

func TestJellyfinNotifications(t *testing.T) {
	var reqs []capturedRequest
	server := mediaBrowserServer(t, &reqs)
	defer server.Close()

	tgt := newMediaBrowserTarget(mediaBrowserConfig{
		flavor: "jellyfin", baseURL: server.URL, apiKey: "secret",
		pathPrefix: "", createType: "Modified", mountPath: "/mnt/wisp",
	}, slog.New(slog.DiscardHandler))

	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")
	tgt.Rename(context.Background(), "series", "shows/Old/e.mkv", "shows/New/e.mkv")
	tgt.Delete(context.Background(), "movie", "movies/New/movie.mkv")

	if len(reqs) != 3 {
		t.Fatalf("request count = %d", len(reqs))
	}
	for i, req := range reqs {
		if req.method != http.MethodPost {
			t.Fatalf("req %d method = %s", i, req.method)
		}
		if req.path != "/Library/Media/Updated" {
			t.Fatalf("req %d path = %s", i, req.path)
		}
		if req.token != "secret" {
			t.Fatalf("req %d token = %q", i, req.token)
		}
	}
	if got := updateTypes(t, reqs[0]); len(got) != 1 || got[0] != "Modified" {
		t.Fatalf("import updateTypes = %v", got)
	}
	if firstUpdatePath(t, reqs[0]) != "/mnt/wisp/movies/New/movie.mkv" {
		t.Fatalf("import path = %s", firstUpdatePath(t, reqs[0]))
	}
	if got := updateTypes(t, reqs[1]); len(got) != 2 || got[0] != "Deleted" || got[1] != "Modified" {
		t.Fatalf("rename updateTypes = %v", got)
	}
	if got := updateTypes(t, reqs[2]); len(got) != 1 || got[0] != "Deleted" {
		t.Fatalf("delete updateTypes = %v", got)
	}
}

func TestEmbyUsesPrefixAndCreated(t *testing.T) {
	var reqs []capturedRequest
	server := mediaBrowserServer(t, &reqs)
	defer server.Close()

	tgt := newMediaBrowserTarget(mediaBrowserConfig{
		flavor: "emby", baseURL: server.URL, apiKey: "k",
		pathPrefix: "/emby", createType: "Created", mountPath: "/mnt/wisp",
	}, slog.New(slog.DiscardHandler))

	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")

	if len(reqs) != 1 {
		t.Fatalf("request count = %d", len(reqs))
	}
	if reqs[0].path != "/emby/Library/Media/Updated" {
		t.Fatalf("emby path = %s", reqs[0].path)
	}
	if got := updateTypes(t, reqs[0]); len(got) != 1 || got[0] != "Created" {
		t.Fatalf("emby import updateTypes = %v", got)
	}
}

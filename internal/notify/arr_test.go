package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArrEventsUseARRPayloadsAndMountPath(t *testing.T) {
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Silo's Autoscan intake keys on method + these headers, so pin them.
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != "wisp" {
			t.Errorf("User-Agent = %q, want wisp", ua)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.Import(context.Background(), "movie", "movies/New/movie.mkv")
	tgt.Rename(context.Background(), "series", "shows/Old/e.mkv", "shows/New/e.mkv")
	tgt.Delete(context.Background(), "movie", "movies/New/movie.mkv")

	if len(payloads) != 3 {
		t.Fatalf("payload count = %d", len(payloads))
	}
	if payloads[0]["eventType"] != "Download" {
		t.Fatalf("import = %#v", payloads[0])
	}
	movieFile := payloads[0]["movieFile"].(map[string]any)
	if movieFile["path"] != "/mnt/wisp/movies/New/movie.mkv" {
		t.Fatalf("import path = %v", movieFile["path"])
	}
	if payloads[1]["eventType"] != "Rename" {
		t.Fatalf("rename = %#v", payloads[1])
	}
	renamed := payloads[1]["renamedEpisodeFiles"].([]any)[0].(map[string]any)
	if renamed["previousPath"] != "/mnt/wisp/shows/Old/e.mkv" || renamed["newPath"] != "/mnt/wisp/shows/New/e.mkv" {
		t.Fatalf("rename paths = %#v", renamed)
	}
	if payloads[2]["eventType"] != "MovieFileDelete" {
		t.Fatalf("delete = %#v", payloads[2])
	}
	movie := payloads[2]["movie"].(map[string]any)
	if movie["folderPath"] != "/mnt/wisp/movies/New" {
		t.Fatalf("delete movie = %#v", movie)
	}
	deletedFile := payloads[2]["movieFile"].(map[string]any)
	if deletedFile["relativePath"] != "movie.mkv" {
		t.Fatalf("delete file = %#v", deletedFile)
	}
}

func TestArrDisabledIsNoop(t *testing.T) {
	tgt := newArrTarget("", "", slog.New(slog.DiscardHandler))
	tgt.Import(context.Background(), "movie", "movies/x.mkv")
	tgt.Rename(context.Background(), "movie", "movies/a.mkv", "movies/b.mkv")
	tgt.Delete(context.Background(), "movie", "movies/b.mkv")
}

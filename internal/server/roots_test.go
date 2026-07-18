package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreulavelle/wisp/internal/library"
)

// An empty wisp must still present all four library roots at the root listing,
// so a media server can validate every library path from boot.
func TestAlwaysPresentRootsEmptyStore(t *testing.T) {
	st := newTestStore(t) // no pins
	srv := New(st, nil, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("root status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, root := range library.Roots() {
		if !strings.Contains(body, `href="`+root+`/"`) {
			t.Fatalf("root listing missing %q: %q", root, body)
		}
	}
}

// A real but empty root directory lists (empty) with 200, not 404 — media
// servers stat the library path at creation.
func TestEmptyRootDirListsNot404(t *testing.T) {
	st := newTestStore(t)
	srv := New(st, nil, discard())

	for _, root := range library.Roots() {
		rec := httptest.NewRecorder()
		srv.FileHandler(rec, getReq(root+"/"))
		if rec.Code != http.StatusOK {
			t.Fatalf("empty root %q status = %d, want 200", root, rec.Code)
		}
	}

	// A deeper path with no pins is still a clean 404.
	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq("anime_shows/Ghost (2099)/"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deep empty dir status = %d, want 404", rec.Code)
	}
}

// The roots merge with real pinned dirs rather than replacing them.
func TestRootsMergeWithPins(t *testing.T) {
	st := newTestStore(t)
	pinEpisode(t, st, "http://x", 1) // creates a shows/… pin
	srv := New(st, nil, discard())

	rec := httptest.NewRecorder()
	srv.FileHandler(rec, getReq(""))
	body := rec.Body.String()
	// shows appears once (pinned + root deduped), and the empty roots appear too.
	if strings.Count(body, `href="shows/"`) != 1 {
		t.Fatalf("shows/ not deduped: %q", body)
	}
	if !strings.Contains(body, `href="anime_movies/"`) {
		t.Fatalf("missing empty anime_movies root: %q", body)
	}
}

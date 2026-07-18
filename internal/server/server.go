// Package server serves the virtual media tree over HTTP. Directory listings
// and file stats come from the pin store; file bytes are range-proxied from
// the pinned AIOStreams resolver URL, which re-unlocks debrid on every open.
// A failed upstream triggers a re-resolve so playback self-heals.
package server

import (
	"context"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/store"
)

// ReResolve refreshes a pin's SourceURL/Size in place (e.g. by re-searching
// AIOStreams) when its current upstream fails. It persists the new values.
type ReResolve func(ctx context.Context, p *store.Pin) error

// Server is the virtual-file HTTP handler.
type Server struct {
	store     *store.Store
	reresolve ReResolve
	client    *http.Client
	log       *slog.Logger
}

// New builds a file server.
func New(st *store.Store, reresolve ReResolve, log *slog.Logger) *Server {
	return &Server{
		store:     st,
		reresolve: reresolve,
		client:    &http.Client{Timeout: 0}, // streaming; no whole-body deadline
		log:       log,
	}
}

// FileHandler serves the virtual tree. Mount it as the catch-all route.
func (s *Server) FileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.Trim(cleanPath(r.URL.Path), "/")

	pin, err := s.store.ByPath(r.Context(), rel)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if pin != nil {
		s.serveFile(w, r, pin)
		return
	}
	s.serveDir(w, r, rel)
}

func (s *Server) serveDir(w http.ResponseWriter, r *http.Request, rel string) {
	dirs, files, err := s.store.Children(r.Context(), rel)
	if err != nil {
		http.Error(w, "listing failed", http.StatusInternalServerError)
		return
	}
	if rel != "" && len(dirs) == 0 && len(files) == 0 {
		http.NotFound(w, r)
		return
	}
	sort.Strings(dirs)
	sort.Strings(files)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	var b strings.Builder
	b.WriteString("<html><body>\n")
	for _, d := range dirs {
		b.WriteString(fmt.Sprintf("<a href=\"%s/\">%s/</a><br>\n", url.PathEscape(d), html.EscapeString(d)))
	}
	for _, f := range files {
		b.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a><br>\n", url.PathEscape(f), html.EscapeString(f)))
	}
	b.WriteString("</body></html>\n")
	io.WriteString(w, b.String())
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, pin *store.Pin) {
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", pin.Size))
		return
	}

	// One retry: if the pinned upstream is gone, re-resolve and try again.
	for attempt := 0; attempt < 2; attempt++ {
		ok, retriable := s.proxyOnce(w, r, pin)
		if ok || !retriable {
			return
		}
		if s.reresolve == nil {
			break
		}
		s.log.Warn("upstream failed; re-resolving", "path", pin.VirtualPath)
		if err := s.reresolve(r.Context(), pin); err != nil {
			s.log.Error("re-resolve failed", "path", pin.VirtualPath, "error", err)
			break
		}
	}
	http.Error(w, "stream temporarily unavailable", http.StatusBadGateway)
}

// proxyOnce streams the upstream once. ok reports success (response written);
// retriable reports whether a re-resolve is worth attempting. Once any bytes
// (or headers) have been written to w, retriable is false.
func (s *Server) proxyOnce(w http.ResponseWriter, r *http.Request, pin *store.Pin) (ok, retriable bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.SourceURL, nil)
	if err != nil {
		cancel()
		return false, false
	}
	req.Header.Set("User-Agent", "wisp")
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		cancel()
		return false, true
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
		resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		resp.Body.Close()
		cancel()
		return false, true
	}
	// Commit: mirror upstream status and content headers, then stream.
	copyHeader(w.Header(), resp.Header, "Content-Type", "Content-Length", "Content-Range", "Accept-Ranges")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "video/x-matroska")
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(w, resp.Body)
	resp.Body.Close()
	cancel()
	if copyErr != nil {
		s.log.Debug("client stream ended", "path", pin.VirtualPath, "error", copyErr)
	}
	return true, false
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + p)
}

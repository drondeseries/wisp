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
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dreulavelle/wisp/internal/store"
)

// ReResolve refreshes a pin's SourceURL/Size in place (e.g. by re-searching
// AIOStreams) when its current upstream fails. It persists the new values.
type ReResolve func(ctx context.Context, p *store.Pin) error

// linkTTL bounds how long a resolved CDN URL is reused before it's re-resolved
// through the permalink. Debrid links stay valid well past this; the cap only
// bounds staleness.
const linkTTL = 15 * time.Minute

type cachedLink struct {
	url     string
	expires time.Time
}

// Server is the virtual-file HTTP handler.
type Server struct {
	store     *store.Store
	reresolve ReResolve
	client    *http.Client
	log       *slog.Logger

	// linkCache maps a virtual path to the CDN URL its permalink last resolved
	// to. Reusing it skips the per-request torrentio/AIOStreams redirect, which
	// dominates stream-start latency (ffmpeg's probe makes many small reads).
	linkMu    sync.Mutex
	linkCache map[string]cachedLink
}

// New builds a file server. The upstream client has no overall timeout — media
// bodies stream for the length of playback — but bounds the parts that must be
// fast (connect, TLS, waiting for response headers) so a dead upstream fails
// quickly instead of hanging a player. Connection pooling keeps concurrent
// range reads (rclone prefetch, multiple viewers) responsive.
func New(st *store.Store, reresolve ReResolve, log *slog.Logger) *Server {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Server{
		store:     st,
		reresolve: reresolve,
		client:    &http.Client{Transport: transport},
		log:       log,
		linkCache: make(map[string]cachedLink),
	}
}

func (s *Server) cachedLink(path string) (string, bool) {
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	if c, ok := s.linkCache[path]; ok && time.Now().Before(c.expires) {
		return c.url, true
	}
	return "", false
}

func (s *Server) storeLink(path, cdnURL string) {
	s.linkMu.Lock()
	s.linkCache[path] = cachedLink{url: cdnURL, expires: time.Now().Add(linkTTL)}
	s.linkMu.Unlock()
}

func (s *Server) invalidateLink(path string) {
	s.linkMu.Lock()
	delete(s.linkCache, path)
	s.linkMu.Unlock()
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

	s.log.Debug("serving", "path", pin.VirtualPath, "range", r.Header.Get("Range"))

	// 1. Fast path: reuse the cached CDN URL, skipping the permalink redirect
	//    that dominates stream-start latency.
	if cdn, ok := s.cachedLink(pin.VirtualPath); ok {
		committed, retriable := s.proxyOnce(w, r, cdn, pin, false)
		if committed {
			return
		}
		if !retriable {
			return // client gone mid-request
		}
		s.invalidateLink(pin.VirtualPath) // stale/expired CDN URL
	}

	// 2. Resolve through the permalink; a successful response caches the CDN URL.
	committed, retriable := s.proxyOnce(w, r, pin.SourceURL, pin, true)
	if committed {
		return
	}
	if !retriable {
		return
	}

	// 3. Permalink itself is dead → re-resolve via AIOStreams and retry once.
	if s.reresolve != nil {
		s.log.Warn("upstream unavailable; re-resolving", "path", pin.VirtualPath)
		if err := s.reresolve(r.Context(), pin); err != nil {
			s.log.Error("re-resolve failed", "path", pin.VirtualPath, "error", err)
		} else {
			s.invalidateLink(pin.VirtualPath)
			if committed, _ := s.proxyOnce(w, r, pin.SourceURL, pin, true); committed {
				s.log.Info("recovered after re-resolve", "path", pin.VirtualPath)
				return
			}
		}
	}
	s.log.Error("stream unavailable after re-resolve", "path", pin.VirtualPath)
	http.Error(w, "stream temporarily unavailable", http.StatusBadGateway)
}

// proxyOnce streams upstreamURL once. committed reports the response was written
// to the client (success); retriable reports whether the failure is worth
// another upstream. When cacheResolved is set, a successful response caches the
// final URL (after redirects) as the pin's CDN URL, so later reads skip the
// permalink hop.
//
// The request rides the client's context (the player's connection); there is no
// artificial body deadline, so playback and long seeks are never truncated.
// Stuck upstreams are caught by the transport's connect/header timeouts.
func (s *Server) proxyOnce(w http.ResponseWriter, r *http.Request, upstreamURL string, pin *store.Pin, cacheResolved bool) (committed, retriable bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("User-Agent", "wisp")
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// Client gone: don't burn a re-resolve on a cancelled request.
		return false, r.Context().Err() == nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
		resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		resp.Body.Close()
		return false, true
	}
	// Cache the resolved CDN URL (final request URL after any redirects) so
	// subsequent reads for this file bypass the permalink.
	if cacheResolved && resp.Request != nil && resp.Request.URL != nil {
		if final := resp.Request.URL.String(); final != "" && final != upstreamURL {
			s.storeLink(pin.VirtualPath, final)
		}
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

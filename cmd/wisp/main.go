// Command wisp serves a resolver-backed virtual media library that any media
// server (Silo, Plex, Jellyfin, Emby) can scan and play as if it were local.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/config"
	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/mount"
	"github.com/dreulavelle/wisp/internal/server"
	"github.com/dreulavelle/wisp/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	aio := aiostreams.New(cfg.AIOStreamsURL, cfg.AIOStreamsPassword)
	app := &app{store: st, aio: aio, log: log, mountPath: cfg.MountPath, startedAt: time.Now()}

	srv := server.New(st, app.reResolve, log)
	app.srv = srv

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/add", app.handleAdd)
	mux.HandleFunc("GET /api/pins", app.handleListPins)
	mux.HandleFunc("DELETE /api/pins", app.handleDeletePin)
	mux.HandleFunc("GET /api/status", app.handleStatus)
	mux.HandleFunc("GET /metrics", app.handleMetrics)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", srv.FileHandler)

	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		log.Info("wisp listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("serve", "error", err)
			os.Exit(1)
		}
	}()

	var mnt *mount.Mount
	if cfg.SelfMount() {
		m, err := mount.Start(context.Background(), mount.Options{
			ServerURL:          "http://127.0.0.1" + portOf(cfg.ListenAddr),
			Mountpoint:         cfg.MountPath,
			AllowOther:         cfg.MountAllowOther,
			ReadChunkSize:      32 << 20,  // 32M first read: snappy seeks
			ReadChunkSizeLimit: 512 << 20, // ramp for sequential playback
		}, log)
		if err != nil {
			log.Error("self-mount failed", "error", err)
			os.Exit(1)
		}
		mnt = m
		app.mnt = m
	} else {
		log.Info("serving HTTP only; mount it with rclone (set WISP_MOUNT_PATH to self-mount)")
	}

	// Graceful shutdown: unmount before exit so the mountpoint isn't left stale.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	if mnt != nil {
		if err := mnt.Close(); err != nil {
			log.Warn("unmount", "error", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// parseLevel maps a config string to a slog level, defaulting to info.
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// portOf extracts ":8080" from a listen address like ":8080" or "0.0.0.0:8080".
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":8080"
}

const version = "0.2.0"

type app struct {
	store     *store.Store
	aio       *aiostreams.Client
	log       *slog.Logger
	srv       *server.Server
	mnt       *mount.Mount
	mountPath string
	startedAt time.Time
}

type addRequest struct {
	MediaType string `json:"media_type"` // "movie" | "series"
	IMDbID    string `json:"imdb_id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	Quality   string `json:"quality"`
	TMDbID    string `json:"tmdb_id"`
	TVDbID    string `json:"tvdb_id"`
}

func (a *app) handleAdd(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.MediaType != "movie" && req.MediaType != "series" {
		http.Error(w, "media_type must be movie or series", http.StatusBadRequest)
		return
	}
	if req.IMDbID == "" || req.Title == "" {
		http.Error(w, "imdb_id and title are required", http.StatusBadRequest)
		return
	}

	sourceURL, size, filename, resolution, err := a.resolve(r.Context(), req.MediaType, req.IMDbID, req.Season, req.Episode)
	if err != nil {
		http.Error(w, "no playable stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Label with AIOStreams' own parsed resolution. Fall back to a filename
	// scan only if it's absent, then the caller's hint, then 1080p. (Silo reads
	// real metadata regardless — this only names the file.)
	quality := resolution
	if quality == "" {
		quality = library.DetectQuality(filename)
	}
	if quality == "" {
		quality = req.Quality
	}
	if quality == "" {
		quality = "1080p"
	}
	ids := library.IDs{IMDb: req.IMDbID, TMDb: req.TMDbID, TVDb: req.TVDbID}
	// If a feeder gave only an IMDb id, enrich TVDB/TMDB ids from Cinemeta so the
	// folder tag lets the media server match deterministically (Silo/Plex/
	// Jellyfin resolve by tvdb/tmdb, not imdb).
	if ids.TVDb == "" && ids.TMDb == "" && strings.HasPrefix(req.IMDbID, "tt") {
		tvdb, tmdb := metadata.ProviderIDs(r.Context(), req.MediaType, req.IMDbID)
		ids.TVDb, ids.TMDb = tvdb, tmdb
	}
	ext := library.Ext(filename)
	var vpath string
	if req.MediaType == "movie" {
		vpath = library.MoviePath(req.Title, req.Year, ids, quality, ext)
	} else {
		vpath = library.EpisodePath(req.Title, req.Year, req.Season, req.Episode, ids, quality, ext)
	}
	pin := store.Pin{
		MediaType: req.MediaType, IMDbID: req.IMDbID, Season: req.Season, Episode: req.Episode,
		Title: req.Title, Year: req.Year, Quality: quality, VirtualPath: vpath,
		SourceURL: sourceURL, Size: size, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(r.Context(), pin); err != nil {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	a.log.Info("pinned", "path", vpath, "size", size)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"virtual_path": vpath, "size": size})
}

func (a *app) handleListPins(w http.ResponseWriter, r *http.Request) {
	pins, err := a.store.List(r.Context())
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(pins))
	for _, p := range pins {
		out = append(out, map[string]any{
			"virtual_path": p.VirtualPath, "media_type": p.MediaType, "imdb_id": p.IMDbID,
			"season": p.Season, "episode": p.Episode, "title": p.Title, "year": p.Year,
			"quality": p.Quality, "size": p.Size, "resolved_at": p.ResolvedAt.Unix(),
		})
	}
	writeJSON(w, out)
}

func (a *app) handleDeletePin(w http.ResponseWriter, r *http.Request) {
	if path := strings.TrimSpace(r.URL.Query().Get("path")); path != "" {
		existed, err := a.store.Delete(r.Context(), path)
		if err != nil {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		if !existed {
			http.NotFound(w, r)
			return
		}
		a.log.Info("deleted", "path", path)
		writeJSON(w, map[string]any{"deleted": []string{path}})
		return
	}
	var req struct {
		IMDbID  string `json:"imdb_id"`
		Season  int    `json:"season"`
		Episode int    `json:"episode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IMDbID == "" {
		http.Error(w, "provide ?path= or a JSON body with imdb_id", http.StatusBadRequest)
		return
	}
	deleted, err := a.store.DeleteByMedia(r.Context(), req.IMDbID, req.Season, req.Episode)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	a.log.Info("deleted", "imdb", req.IMDbID, "count", len(deleted))
	writeJSON(w, map[string]any{"deleted": deleted})
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	count, _ := a.store.Count(r.Context())
	writeJSON(w, map[string]any{
		"version":        version,
		"uptime_seconds": int(time.Since(a.startedAt).Seconds()),
		"pins":           count,
		"mounted":        a.mnt.Healthy(),
		"mount_path":     a.mountPath,
	})
}

func (a *app) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := a.srv.Metrics()
	pins, _ := a.store.Count(r.Context())
	mounted := 0
	if a.mnt.Healthy() {
		mounted = 1
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metric := func(name, help, typ string, val int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, val)
	}
	metric("wisp_pins", "Pinned media files.", "gauge", int64(pins))
	metric("wisp_mounted", "FUSE mount live (1) or not (0).", "gauge", int64(mounted))
	metric("wisp_uptime_seconds", "Process uptime.", "gauge", int64(time.Since(a.startedAt).Seconds()))
	metric("wisp_file_requests_total", "Byte-range file requests served.", "counter", m.FileRequests)
	metric("wisp_link_cache_hits_total", "CDN URL cache hits.", "counter", m.CacheHits)
	metric("wisp_link_cache_misses_total", "CDN URL cache misses (permalink resolves).", "counter", m.CacheMisses)
	metric("wisp_reresolves_total", "Self-heal re-resolves via AIOStreams.", "counter", m.ReResolves)
	metric("wisp_link_cache_entries", "Cached CDN URLs currently held.", "gauge", int64(m.LinkCacheSize))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// reResolve refreshes a pin whose upstream failed by re-searching AIOStreams.
func (a *app) reResolve(ctx context.Context, p *store.Pin) error {
	sourceURL, size, _, _, err := a.resolve(ctx, p.MediaType, p.IMDbID, p.Season, p.Episode)
	if err != nil {
		return err
	}
	p.SourceURL, p.Size = sourceURL, size
	return a.store.UpdateResolution(ctx, p.VirtualPath, sourceURL, size)
}

// resolve picks the top-ranked playable stream and measures its size.
func (a *app) resolve(ctx context.Context, mediaType, imdbID string, season, episode int) (sourceURL string, size int64, filename, resolution string, err error) {
	streams, err := a.aio.Search(ctx, mediaType, imdbID, season, episode)
	if err != nil {
		return "", 0, "", "", err
	}
	if len(streams) == 0 {
		return "", 0, "", "", fmt.Errorf("no results")
	}
	top := streams[0]
	size, err = headSize(ctx, top.URL)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("size probe: %w", err)
	}
	return top.URL, size, top.Filename, top.Resolution, nil
}

// headSize follows redirects to the CDN and returns the reported size.
func headSize(ctx context.Context, rawURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "wisp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("upstream did not report a size (HTTP %d)", resp.StatusCode)
	}
	return resp.ContentLength, nil
}

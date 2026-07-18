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
	"github.com/dreulavelle/wisp/internal/mount"
	"github.com/dreulavelle/wisp/internal/server"
	"github.com/dreulavelle/wisp/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "error", err)
		os.Exit(1)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	aio := aiostreams.New(cfg.AIOStreamsURL, cfg.AIOStreamsPassword)
	app := &app{store: st, aio: aio, log: log}

	srv := server.New(st, app.reResolve, log)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/add", app.handleAdd)
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

// portOf extracts ":8080" from a listen address like ":8080" or "0.0.0.0:8080".
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":8080"
}

type app struct {
	store *store.Store
	aio   *aiostreams.Client
	log   *slog.Logger
}

type addRequest struct {
	MediaType string `json:"media_type"` // "movie" | "series"
	IMDbID    string `json:"imdb_id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	Quality   string `json:"quality"`
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
	if req.Quality == "" {
		req.Quality = "1080p"
	}

	sourceURL, size, filename, err := a.resolve(r.Context(), req.MediaType, req.IMDbID, req.Season, req.Episode)
	if err != nil {
		http.Error(w, "no playable stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	ext := library.Ext(filename)
	var vpath string
	if req.MediaType == "movie" {
		vpath = library.MoviePath(req.Title, req.Year, req.Quality, ext)
	} else {
		vpath = library.EpisodePath(req.Title, req.Year, req.Season, req.Episode, req.Quality, ext)
	}
	pin := store.Pin{
		MediaType: req.MediaType, IMDbID: req.IMDbID, Season: req.Season, Episode: req.Episode,
		Title: req.Title, Year: req.Year, Quality: req.Quality, VirtualPath: vpath,
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

// reResolve refreshes a pin whose upstream failed by re-searching AIOStreams.
func (a *app) reResolve(ctx context.Context, p *store.Pin) error {
	sourceURL, size, _, err := a.resolve(ctx, p.MediaType, p.IMDbID, p.Season, p.Episode)
	if err != nil {
		return err
	}
	p.SourceURL, p.Size = sourceURL, size
	return a.store.UpdateResolution(ctx, p.ID, sourceURL, size)
}

// resolve picks the top-ranked playable stream and measures its size.
func (a *app) resolve(ctx context.Context, mediaType, imdbID string, season, episode int) (sourceURL string, size int64, filename string, err error) {
	streams, err := a.aio.Search(ctx, mediaType, imdbID, season, episode)
	if err != nil {
		return "", 0, "", err
	}
	if len(streams) == 0 {
		return "", 0, "", fmt.Errorf("no results")
	}
	top := streams[0]
	size, err = headSize(ctx, top.URL)
	if err != nil {
		return "", 0, "", fmt.Errorf("size probe: %w", err)
	}
	return top.URL, size, top.Filename, nil
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

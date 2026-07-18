// Command wisp serves a resolver-backed virtual media library that any media
// server (Silo, Plex, Jellyfin, Emby) can scan and play as if it were local.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/config"
	"github.com/dreulavelle/wisp/internal/library"
	"github.com/dreulavelle/wisp/internal/metadata"
	"github.com/dreulavelle/wisp/internal/monitor"
	"github.com/dreulavelle/wisp/internal/mount"
	"github.com/dreulavelle/wisp/internal/notify"
	"github.com/dreulavelle/wisp/internal/server"
	"github.com/dreulavelle/wisp/internal/store"
	"github.com/rclone/rclone/fs"
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
	warnDBPersistence(cfg.DBPath, log)

	// Additive, idempotent migration: stamp a Category on pins/monitors that
	// predate the field, from their existing paths. Never rewrites VirtualPaths.
	if n, err := st.BackfillCategories(context.Background()); err != nil {
		log.Warn("category backfill", "error", err)
	} else if n > 0 {
		log.Info("category backfill", "records_updated", n)
	}

	notifier := notify.New(notify.Options{
		ArrWebhookURL:  cfg.NotifyArrWebhookURL,
		SiloWebhookURL: cfg.SiloWebhookURL,
		JellyfinURL:    cfg.NotifyJellyfinURL, JellyfinAPIKey: cfg.NotifyJellyfinAPIKey,
		EmbyURL: cfg.NotifyEmbyURL, EmbyAPIKey: cfg.NotifyEmbyAPIKey,
		PlexURL: cfg.NotifyPlexURL, PlexToken: cfg.NotifyPlexToken,
		MountPath: cfg.MountPath,
	}, log)
	if targets := notifier.Targets(); len(targets) > 0 {
		log.Info("media-server notifications enabled", "targets", targets)
	}

	aio := aiostreams.New(cfg.AIOStreamsURL, cfg.AIOStreamsPassword)
	if !aio.HasCredentials() {
		log.Warn("no AIOStreams password set; the Search API needs Basic auth unless your instance allows unauthenticated requests — set WISP_AIOSTREAMS_PASSWORD if adds return aiostreams_auth")
	}
	app := &app{
		store: st, aio: aio, log: log, mountPath: cfg.MountPath,
		webhook:   notifier,
		meta:      metadata.New(cfg.TMDBAPIKey, cfg.TMDBMarkets),
		startedAt: time.Now(),
	}
	app.mon = monitor.New(st, app.meta, app, cfg.ScheduleInterval, log)

	srv := server.New(st, app.reResolve, log)
	app.srv = srv

	// The monitor scheduler runs for the process lifetime, pinning released
	// movies and newly-aired episodes; it wakes near the next airstamp.
	monCtx, monCancel := context.WithCancel(context.Background())
	defer monCancel()
	go app.mon.Run(monCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/add", app.handleAdd)
	mux.HandleFunc("GET /api/pins", app.handleListPins)
	mux.HandleFunc("DELETE /api/pins", app.handleDeletePin)
	mux.HandleFunc("POST /api/monitors", app.handleCreateMonitor)
	mux.HandleFunc("GET /api/monitors", app.handleListMonitors)
	mux.HandleFunc("DELETE /api/monitors", app.handleDeleteMonitor)
	mux.HandleFunc("POST /api/monitors/refresh", app.handleRefreshMonitors)
	mux.HandleFunc("GET /api/schedule", app.handleSchedule)
	mux.HandleFunc("GET /api/requests/status", app.handleRequestStatus)
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
			ReadChunkSize:      cfg.ReadChunkSize,
			ReadChunkSizeLimit: cfg.ReadChunkSizeLimit,
			Delete:             app.deleteMountedPin,
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

const version = "0.7.1" // x-release-please-version

type app struct {
	store     *store.Store
	aio       *aiostreams.Client
	log       *slog.Logger
	srv       *server.Server
	mnt       *mount.Mount
	webhook   notify.Notifier
	meta      *metadata.Service
	mon       *monitor.Monitor
	mountPath string
	startedAt time.Time
	// pinMu serializes the store-mutation half of pin() (list → upsert → delete
	// of superseded pins) so concurrent pins — API and scheduler — can't clobber
	// each other. The network resolve runs outside it.
	pinMu sync.Mutex
}

type addRequest struct {
	MediaType string `json:"media_type"` // "movie" | "series"
	IMDbID    string `json:"imdb_id"`
	TMDbID    string `json:"tmdb_id"`
	TVDbID    string `json:"tvdb_id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`

	// Legacy direct-pin fields: resolve and pin one file synchronously.
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Quality string `json:"quality"`

	// Request-shaped intake fields: register/update a monitor (the async path the
	// Silo plugin shim calls). Their presence routes the request to intake.
	// Qualities is a pointer so a present-but-empty `"qualities": []` (meaning
	// "best available") is distinguishable from an absent field.
	IsAnime    *bool          `json:"is_anime"`
	Qualities  *[]qualitySpec `json:"qualities"`
	RequestRef string         `json:"request_ref"`
}

// qualitySpec is one requested tier in a request-shaped add. id is a resolution
// label ("1080p"|"2160p"); is4k is a fallback hint when id is absent.
type qualitySpec struct {
	ID   string `json:"id"`
	Is4K bool   `json:"is4k"`
}

// isRequestShaped reports whether the body uses the request-shaped intake
// contract rather than a legacy direct-pin. The presence of the `qualities`
// field (even as an empty array), `request_ref`, `is_anime`, or a tmdb-only
// identity the legacy synchronous path can't pin all mark a request. Existing
// simpler payloads (imdb + season/episode/quality) fall through to the legacy
// path unchanged.
func (r addRequest) isRequestShaped() bool {
	return r.Qualities != nil || r.RequestRef != "" || r.IsAnime != nil ||
		(r.IMDbID == "" && r.TMDbID != "")
}

// qualities maps the request-shaped tiers to wisp's canonical quality labels,
// deduped and order-preserving. An absent or empty field yields no tiers ("best
// available"). An unrecognized id falls back to 2160p when its is4k hint is set;
// otherwise it is dropped.
func (r addRequest) qualities() []string {
	if r.Qualities == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, q := range *r.Qualities {
		label := library.NormalizeQuality(q.ID)
		if label == "" && q.Is4K {
			label = "2160p"
		}
		if label != "" && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
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
	if req.isRequestShaped() {
		a.handleAddRequest(w, r, req)
		return
	}
	// Legacy synchronous direct-pin path: resolve and pin one file now.
	if req.IMDbID == "" || req.Title == "" {
		http.Error(w, "imdb_id and title are required", http.StatusBadRequest)
		return
	}
	vpath, size, err := a.pin(r.Context(), pinSpec{
		MediaType: req.MediaType, IMDbID: req.IMDbID, TMDbID: req.TMDbID, TVDbID: req.TVDbID,
		Title: req.Title, Year: req.Year, Season: req.Season, Episode: req.Episode, Quality: req.Quality,
	})
	if err != nil {
		writeAddError(w, a.log, req, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"virtual_path": vpath, "size": size})
}

// handleAddRequest registers/updates a monitor from a request-shaped body and
// returns immediately (202). It never resolves synchronously — the scheduler
// does enumeration, release gating, and stream resolution. Idempotent per title:
// re-posting extends the existing monitor rather than duplicating it.
func (a *app) handleAddRequest(w http.ResponseWriter, r *http.Request, req addRequest) {
	if req.IMDbID == "" && req.TMDbID == "" {
		http.Error(w, "imdb_id or tmdb_id is required", http.StatusBadRequest)
		return
	}
	if err := a.mon.Intake(r.Context(), monitor.Request{
		MediaType: req.MediaType, IMDbID: req.IMDbID, TMDbID: req.TMDbID, TVDbID: req.TVDbID,
		Title: req.Title, Year: req.Year, Qualities: req.qualities(),
		IsAnime: req.IsAnime, RequestRef: req.RequestRef,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"monitoring": true, "state": statusQueued})
}

// pinSpec is one resolve+pin request, shared by the API and the monitor.
type pinSpec struct {
	MediaType string
	IMDbID    string
	TMDbID    string
	TVDbID    string
	Title     string
	Year      int
	Season    int
	Episode   int
	Quality   string
	// Category is the library root the title resolved to (library.Root*). The
	// monitor supplies it (decided once at intake); the legacy direct-pin path
	// leaves it empty and pin() inherits it from an existing pin/monitor, else
	// defaults to non-anime — never re-deriving an existing title's root.
	Category string
}

// pin resolves a stream via AIOStreams and records the pin, superseding a
// same-quality pin at a different path and notifying the media server. A movie
// or series without an IMDb id is searched (and tagged) by its tmdb id. The
// returned error is a classified resolve error (errNo… or *aiostreams.SearchError).
func (a *app) pin(ctx context.Context, s pinSpec) (vpath string, size int64, err error) {
	searchID := s.IMDbID
	if searchID == "" && s.TMDbID != "" {
		searchID = "tmdb:" + s.TMDbID
	}
	wantQuality := library.NormalizeQuality(s.Quality)
	sourceURL, size, filename, resolution, err := a.resolve(ctx, s.MediaType, searchID, s.Season, s.Episode, wantQuality)
	if err != nil {
		return "", 0, err
	}
	quality := qualityLabel(resolution, filename, wantQuality)
	ids := library.IDs{IMDb: searchID, TMDb: s.TMDbID, TVDb: s.TVDbID}
	// If only an IMDb id is known, enrich tvdb/tmdb from Cinemeta so the folder
	// tag lets the media server match deterministically (they resolve by
	// tvdb/tmdb, not imdb).
	if ids.TVDb == "" && ids.TMDb == "" && strings.HasPrefix(searchID, "tt") {
		tvdb, tmdb := metadata.ProviderIDs(ctx, s.MediaType, searchID)
		ids.TVDb, ids.TMDb = tvdb, tmdb
	}
	// The category (library root) is decided once per title and inherited by all
	// its pins; never re-derive it — the root is part of VirtualPath (the key).
	root := s.Category
	if root == "" {
		root = a.inheritCategory(ctx, searchID, s.MediaType)
	}
	ext := library.Ext(filename)
	if s.MediaType == "movie" {
		vpath = library.MoviePath(root, s.Title, s.Year, ids, quality, ext)
	} else {
		vpath = library.EpisodePath(root, s.Title, s.Year, s.Season, s.Episode, ids, quality, ext)
	}
	pin := store.Pin{
		MediaType: s.MediaType, IMDbID: searchID, TMDbID: ids.TMDb, TVDbID: ids.TVDb,
		Category: root, Season: s.Season, Episode: s.Episode,
		Title: s.Title, Year: s.Year, Quality: quality, VirtualPath: vpath,
		SourceURL: sourceURL, Size: size, ResolvedAt: time.Now(),
	}
	// Serialize the store mutation so concurrent pins (API + scheduler) can't race
	// on list→upsert→delete. A supersede/rename is the *same quality tier* landing
	// at a new path (e.g. a re-resolve changed the extension); pins that differ
	// only by quality are distinct targets and must coexist, so quality is part of
	// the identity here.
	a.pinMu.Lock()
	existing, _ := a.store.List(ctx)
	var renamedPaths []string
	for _, old := range existing {
		if old.IMDbID == searchID && old.Season == s.Season && old.Episode == s.Episode &&
			strings.EqualFold(old.Quality, quality) && old.VirtualPath != vpath {
			renamedPaths = append(renamedPaths, old.VirtualPath)
		}
	}
	if err := a.store.Upsert(ctx, pin); err != nil {
		a.pinMu.Unlock()
		return "", 0, err
	}
	var superseded []string
	for _, oldPath := range renamedPaths {
		if deleted, e := a.store.Delete(ctx, oldPath); e != nil {
			a.log.Warn("remove renamed pin", "path", oldPath, "error", e)
		} else if deleted {
			superseded = append(superseded, oldPath)
		}
	}
	a.pinMu.Unlock()

	a.log.Info("pinned", "path", vpath, "size", size)
	// Notify the media server outside the lock (network, best-effort).
	for _, oldPath := range superseded {
		a.webhook.Rename(ctx, s.MediaType, oldPath, vpath)
	}
	if len(renamedPaths) == 0 {
		a.webhook.Import(ctx, s.MediaType, vpath)
	}
	return vpath, size, nil
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
		existed, err := a.deletePin(r.Context(), path)
		if err != nil {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		if !existed {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"deleted": []string{path}})
		return
	}
	var req struct {
		IMDbID  string `json:"imdb_id"`
		Season  int    `json:"season"`
		Episode int    `json:"episode"`
		Quality string `json:"quality"` // optional: delete only this quality tier
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IMDbID == "" {
		http.Error(w, "provide ?path= or a JSON body with imdb_id", http.StatusBadRequest)
		return
	}
	// An empty quality means "all tiers"; a non-empty one must resolve to a known
	// tier, or we'd silently widen a targeted delete into a delete-all.
	quality := library.NormalizeQuality(req.Quality)
	if strings.TrimSpace(req.Quality) != "" && quality == "" {
		http.Error(w, "unrecognized quality", http.StatusBadRequest)
		return
	}
	deleted, err := a.store.DeleteByMedia(r.Context(), req.IMDbID, req.Season, req.Episode, quality)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	a.log.Info("deleted", "imdb", req.IMDbID, "quality", req.Quality, "count", len(deleted))
	writeJSON(w, map[string]any{"deleted": deleted})
	for _, path := range deleted {
		a.webhook.Delete(r.Context(), mediaTypeForPath(path), path)
	}
}

// inheritCategory returns the library root a title's pins already use — from an
// existing pin, else the title's monitor — so a legacy direct pin lands under
// the same root the title was first categorized into (first-writer-wins). It
// falls back to the non-anime root for the media type when nothing is known,
// preserving the pre-category layout for brand-new direct adds.
func (a *app) inheritCategory(ctx context.Context, searchID, mediaType string) string {
	if pins, err := a.store.PinsByMedia(ctx, searchID); err == nil {
		for _, p := range pins {
			if p.Category != "" {
				return p.Category
			}
			if root := library.RootOf(p.VirtualPath); root != "" {
				return root
			}
		}
	}
	// monitorKey mirrors the monitor package: mediaType + ":" + searchID.
	if mon, err := a.store.GetMonitored(ctx, mediaType+":"+searchID); err == nil && mon != nil && mon.Category != "" {
		return mon.Category
	}
	return library.Root(mediaType, false)
}

func mediaTypeForPath(virtualPath string) string {
	return library.MediaTypeForRoot(library.RootOf(virtualPath))
}

func (a *app) deletePin(ctx context.Context, path string) (bool, error) {
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	existed, err := a.store.Delete(ctx, path)
	if err != nil || !existed {
		return existed, err
	}
	a.log.Info("deleted", "path", path)
	// Fire from the shared helper so both the API and a mounted `rm` notify Silo.
	// The bulk (imdb_id) delete path uses DeleteByMedia directly and emits its own
	// events, so this does not double-fire.
	a.webhook.Delete(ctx, mediaTypeForPath(path), path)
	return true, nil
}

func (a *app) deleteMountedPin(ctx context.Context, path string) error {
	existed, err := a.deletePin(ctx, path)
	if err != nil {
		return err
	}
	if !existed {
		return fs.ErrorObjectNotFound
	}
	return nil
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	count, _ := a.store.Count(r.Context())
	monitors, _ := a.store.CountMonitored(r.Context())
	writeJSON(w, map[string]any{
		"version":        version,
		"uptime_seconds": int(time.Since(a.startedAt).Seconds()),
		"pins":           count,
		"monitors":       monitors,
		"mounted":        a.mnt.Healthy(),
		"mount_path":     a.mountPath,
		"schedule": map[string]any{
			"monitors":         monitors,
			"interval_seconds": int(a.mon.Interval().Seconds()),
		},
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
	monitors, _ := a.store.CountMonitored(r.Context())
	metric("wisp_pins", "Pinned media files.", "gauge", int64(pins))
	metric("wisp_monitors", "Titles on the monitor watchlist.", "gauge", int64(monitors))
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

// Classified resolve outcomes. These map to distinct API responses in
// writeAddError so feeders can tell a genuine no-stream condition from a
// configuration/throttling problem (see aiostreams.SearchError for upstream
// failures, which propagate through resolve unchanged).
var (
	errNoResults      = errors.New("aiostreams returned no results")
	errNoPlayable     = errors.New("no probeable stream among results")
	errNoQualityMatch = errors.New("no stream matches the requested quality")
)

// reResolve refreshes a pin whose upstream failed by re-searching AIOStreams. It
// keeps the pin's quality tier so a self-heal doesn't swap 4K for 1080p under a
// file named [2160p]; if that tier has vanished it falls back to the best
// available so playback still survives.
func (a *app) reResolve(ctx context.Context, p *store.Pin) error {
	sourceURL, size, _, _, err := a.resolve(ctx, p.MediaType, p.IMDbID, p.Season, p.Episode, library.NormalizeQuality(p.Quality))
	if errors.Is(err, errNoQualityMatch) {
		sourceURL, size, _, _, err = a.resolve(ctx, p.MediaType, p.IMDbID, p.Season, p.Episode, "")
	}
	if err != nil {
		return err
	}
	p.SourceURL, p.Size = sourceURL, size
	return a.store.UpdateResolution(ctx, p.VirtualPath, sourceURL, size)
}

// resolve picks the highest-ranked stream whose resolver can serve media and
// report the complete file size. A bad resolver must not hide later results.
// When wantQuality is set (canonical form from library.NormalizeQuality), only
// streams of that resolution are considered, so a caller can pin distinct
// 1080p/2160p files; an empty wantQuality keeps the best-stream behavior.
func (a *app) resolve(ctx context.Context, mediaType, imdbID string, season, episode int, wantQuality string) (sourceURL string, size int64, filename, resolution string, err error) {
	streams, err := a.aio.Search(ctx, mediaType, imdbID, season, episode)
	if err != nil {
		return "", 0, "", "", err
	}
	if len(streams) == 0 {
		return "", 0, "", "", errNoResults
	}
	if wantQuality != "" {
		filtered := filterByResolution(streams, wantQuality)
		if len(filtered) == 0 {
			return "", 0, "", "", errNoQualityMatch
		}
		streams = filtered
	}
	stream, size, err := selectPlayableStream(ctx, streams)
	if err != nil {
		return "", 0, "", "", errNoPlayable
	}
	return stream.URL, size, stream.Filename, stream.Resolution, nil
}

// qualityLabel picks the quality that names a pinned file (and keys its virtual
// path). It canonicalizes AIOStreams' parsed resolution ("4K" → "2160p") when
// recognized, so the label, filterByResolution, and quality-scoped deletion all
// share one vocabulary; it falls back to the raw resolution, then a filename
// scan, the requested quality, and finally 1080p. (Silo reads real metadata
// regardless — this only names the file.)
func qualityLabel(resolution, filename, want string) string {
	if norm := library.NormalizeQuality(resolution); norm != "" {
		return norm
	}
	if resolution != "" {
		return resolution
	}
	if q := library.DetectQuality(filename); q != "" {
		return q
	}
	if want != "" {
		return want
	}
	return "1080p"
}

// filterByResolution keeps only streams whose parsed resolution matches the
// requested (canonical) quality, preserving AIOStreams' ranking order.
func filterByResolution(streams []aiostreams.Stream, wantQuality string) []aiostreams.Stream {
	out := make([]aiostreams.Stream, 0, len(streams))
	for _, s := range streams {
		if library.NormalizeQuality(s.Resolution) == wantQuality {
			out = append(out, s)
		}
	}
	return out
}

// writeAddError maps a resolve failure to a distinct HTTP status + structured
// error code. Genuine no-stream cases stay 502 so a feeder keeps the title
// monitored; auth/rate-limit/transient upstream failures surface as their own
// codes so a configuration or throttling problem isn't masked as unavailability.
// The failure is logged without credentials or resolver URLs.
func writeAddError(w http.ResponseWriter, log *slog.Logger, req addRequest, err error) {
	// Default to a non-502 upstream fault: only the explicit no-stream sentinels
	// map to 502, so an unclassified failure (bad URL, decode error, success:false)
	// surfaces as an error a feeder acts on rather than a monitorable "no stream".
	status, code, message := http.StatusServiceUnavailable, "upstream_unavailable", "AIOStreams unavailable"
	var se *aiostreams.SearchError
	switch {
	case errors.Is(err, errNoQualityMatch):
		status, code, message = http.StatusBadGateway, "no_quality_match", "no stream matches the requested quality"
	case errors.Is(err, errNoResults), errors.Is(err, errNoPlayable):
		status, code, message = http.StatusBadGateway, "no_streams", err.Error()
	case errors.As(err, &se):
		switch se.Kind {
		case aiostreams.KindAuth:
			status, code, message = http.StatusInternalServerError, "aiostreams_auth", "AIOStreams authentication failed; check credentials"
		case aiostreams.KindRateLimited:
			status, code, message = http.StatusTooManyRequests, "rate_limited", "AIOStreams rate limited; retry later"
			if se.RetryAfter > 0 {
				// Round up so a sub-second remainder never advises retrying early.
				secs := int((se.RetryAfter + time.Second - 1) / time.Second)
				w.Header().Set("Retry-After", strconv.Itoa(secs))
			}
		default:
			status, code, message = http.StatusServiceUnavailable, "upstream_unavailable", "AIOStreams temporarily unavailable"
		}
	}
	log.Warn("add failed", "code", code, "media_type", req.MediaType, "imdb", req.IMDbID,
		"season", req.Season, "episode", req.Episode, "detail", err.Error())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "message": message})
}

func selectPlayableStream(ctx context.Context, streams []aiostreams.Stream) (aiostreams.Stream, int64, error) {
	var lastErr error
	for _, stream := range streams {
		size, err := probeSize(ctx, stream.URL)
		if err == nil {
			return stream, size, nil
		}
		lastErr = err
	}
	return aiostreams.Stream{}, 0, fmt.Errorf("all %d results failed probing: %w", len(streams), lastErr)
}

// probeSize uses a one-byte ranged GET because AIOStreams resolver permalinks
// do not support HEAD. For a partial response, Content-Range carries the full
// media size; servers that ignore Range may instead return 200 + Content-Length.
func probeSize(ctx context.Context, rawURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "wisp")
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") {
		return 0, fmt.Errorf("upstream returned non-media content type %q", contentType)
	}
	if resp.StatusCode == http.StatusPartialContent {
		size, err := contentRangeSize(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, err
		}
		return size, nil
	}
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("upstream did not report a size (HTTP %d)", resp.StatusCode)
	}
	return resp.ContentLength, nil
}

func contentRangeSize(value string) (int64, error) {
	_, total, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || total == "" || total == "*" {
		return 0, fmt.Errorf("upstream returned invalid Content-Range %q", value)
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("upstream returned invalid Content-Range %q", value)
	}
	return size, nil
}

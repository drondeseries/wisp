// Package notify tells media servers to rescan when wisp pins, renames, or
// deletes a virtual file. It fans out to any number of configured targets —
// an ARR-compatible webhook (Silo/Sonarr/Radarr), a MediaBrowser server
// (Jellyfin/Emby/Silo's Jellyfin-compat endpoint), or Plex — so one wisp
// instance can drive several libraries at once.
//
// Delivery is fire-and-forget: every target is notified on its own goroutine
// with a detached, timeout-bounded context, so a slow or unreachable media
// server never blocks (or fails) a pin or delete. Failures are logged per
// target and otherwise swallowed.
package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

// notifyTimeout bounds a single target's whole notification (Plex may need a
// section lookup plus a refresh). Individual HTTP clients keep tighter timeouts.
const notifyTimeout = 30 * time.Second

// defaultMountPath is used when no mount path is configured, matching the arr
// webhook's historical default.
const defaultMountPath = "/mnt/wisp"

// Notifier reports pin lifecycle events to media servers. Paths are
// library-relative virtual paths (e.g. "movies/Foo (2020)/Foo.mkv"); each
// target resolves them against the mount path as needed.
type Notifier interface {
	Import(ctx context.Context, mediaType, virtualPath string)
	Rename(ctx context.Context, mediaType, previousPath, newPath string)
	Delete(ctx context.Context, mediaType, virtualPath string)
}

// target is one concrete media server. Each implementation is responsible for
// its own payload shape and transport; the Multi fans events out to all of them.
type target interface {
	Notifier
	// name identifies the target in log lines.
	name() string
}

// Options configures which targets a notifier drives. Empty fields disable the
// corresponding target; a notifier with no targets is a safe no-op.
type Options struct {
	// ArrWebhookURL is the canonical ARR-compatible webhook URL
	// (WISP_NOTIFY_ARR_WEBHOOK_URL).
	ArrWebhookURL string
	// SiloWebhookURL is the deprecated alias (WISP_SILO_WEBHOOK_URL). When both
	// are set the canonical one wins and a note is logged.
	SiloWebhookURL string

	// Jellyfin target (Jellyfin, or Silo's Jellyfin-compatible endpoint).
	JellyfinURL    string
	JellyfinAPIKey string

	// Emby target — same protocol as Jellyfin but routes under /emby and marks
	// new files "Created" per Emby convention.
	EmbyURL    string
	EmbyAPIKey string

	// Plex target.
	PlexURL   string
	PlexToken string

	// MountPath is where the media servers see wisp's library on disk; virtual
	// paths are resolved against it. Empty defaults to /mnt/wisp.
	MountPath string
}

// New builds a Notifier from the configured targets. It always returns a
// non-nil *Multi (with zero or more targets), so callers never need a nil check.
func New(opts Options, log *slog.Logger) *Multi {
	mountPath := strings.TrimSpace(opts.MountPath)
	if mountPath == "" {
		mountPath = defaultMountPath
	}

	arrURL := strings.TrimSpace(opts.ArrWebhookURL)
	if silo := strings.TrimSpace(opts.SiloWebhookURL); silo != "" {
		if arrURL != "" && arrURL != silo {
			log.Info("both WISP_NOTIFY_ARR_WEBHOOK_URL and WISP_SILO_WEBHOOK_URL are set; using the canonical WISP_NOTIFY_ARR_WEBHOOK_URL")
		} else if arrURL == "" {
			arrURL = silo
		}
	}

	m := &Multi{mountPath: mountPath, log: log}
	if arrURL != "" {
		m.targets = append(m.targets, newArrTarget(arrURL, mountPath, log))
	}
	if url := strings.TrimSpace(opts.JellyfinURL); url != "" {
		m.targets = append(m.targets, newMediaBrowserTarget(mediaBrowserConfig{
			flavor: "jellyfin", baseURL: url, apiKey: opts.JellyfinAPIKey,
			pathPrefix: "", createType: "Modified", mountPath: mountPath,
		}, log))
	}
	if url := strings.TrimSpace(opts.EmbyURL); url != "" {
		m.targets = append(m.targets, newMediaBrowserTarget(mediaBrowserConfig{
			flavor: "emby", baseURL: url, apiKey: opts.EmbyAPIKey,
			pathPrefix: "/emby", createType: "Created", mountPath: mountPath,
		}, log))
	}
	if url := strings.TrimSpace(opts.PlexURL); url != "" {
		m.targets = append(m.targets, newPlexTarget(url, opts.PlexToken, mountPath, log))
	}
	return m
}

// Multi fans notifications out to every configured target.
type Multi struct {
	targets   []target
	mountPath string
	log       *slog.Logger
}

// Targets returns the names of the configured targets (for startup logging).
func (m *Multi) Targets() []string {
	names := make([]string, 0, len(m.targets))
	for _, t := range m.targets {
		names = append(names, t.name())
	}
	return names
}

func (m *Multi) Import(ctx context.Context, mediaType, virtualPath string) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.Import(ctx, mediaType, virtualPath) })
}

func (m *Multi) Rename(ctx context.Context, mediaType, previousPath, newPath string) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.Rename(ctx, mediaType, previousPath, newPath) })
}

func (m *Multi) Delete(ctx context.Context, mediaType, virtualPath string) {
	m.fanout(ctx, func(ctx context.Context, t target) { t.Delete(ctx, mediaType, virtualPath) })
}

// fanout runs fn against every target on its own goroutine, detached from the
// caller's context so a returning request handler doesn't cancel delivery.
func (m *Multi) fanout(ctx context.Context, fn func(context.Context, target)) {
	if m == nil || len(m.targets) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	for _, t := range m.targets {
		go func(t target) {
			fctx, cancel := context.WithTimeout(base, notifyTimeout)
			defer cancel()
			fn(fctx, t)
		}(t)
	}
}

// fullPath resolves a library-relative virtual path against the mount path so
// media servers receive the absolute path they see on disk.
func fullPath(mountPath, virtualPath string) string {
	return path.Join(mountPath, strings.TrimLeft(virtualPath, "/"))
}

// postJSON sends a JSON body and drains the response. It returns the status code
// (0 on transport error) so callers can log per-target outcomes.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wisp")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doAndDrain(client, req)
}

// doAndDrain executes req, drains up to 64KiB of the body, and returns the
// status code (0 on transport error).
func doAndDrain(client *http.Client, req *http.Request) (int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// okStatus reports whether a status code is a 2xx success.
func okStatus(code int) bool { return code >= 200 && code < 300 }

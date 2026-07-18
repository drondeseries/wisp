// Package config loads Wisp's runtime settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds everything Wisp needs to serve a resolver-backed library.
type Config struct {
	// AIOStreamsURL is the full Stremio manifest URL of the AIOStreams
	// instance, e.g. https://host/stremio/{uuid}/{blob}/manifest.json or
	// the alias form https://host/stremio/u/{alias}/manifest.json.
	AIOStreamsURL string
	// AIOStreamsPassword is the addon password (paired with the UUID/alias
	// derived from the URL for Search API basic auth).
	AIOStreamsPassword string
	// ListenAddr is the address the virtual-file HTTP server binds to.
	ListenAddr string
	// DBPath is where the pin database lives.
	DBPath string
	// SiloWebhookURL is the deprecated alias for NotifyArrWebhookURL
	// (WISP_SILO_WEBHOOK_URL). It still works; the canonical name wins if both
	// are set.
	SiloWebhookURL string
	// NotifyArrWebhookURL is the canonical ARR-compatible Autoscan webhook
	// (Silo/Sonarr/Radarr shape) notified after imports, renames, and deletions.
	NotifyArrWebhookURL string
	// NotifyJellyfinURL / NotifyJellyfinAPIKey point at a Jellyfin server (or
	// Silo's Jellyfin-compatible endpoint) rescanned via Library/Media/Updated.
	NotifyJellyfinURL    string
	NotifyJellyfinAPIKey string
	// NotifyEmbyURL / NotifyEmbyAPIKey point at an Emby server (same protocol,
	// routed under /emby).
	NotifyEmbyURL    string
	NotifyEmbyAPIKey string
	// NotifyPlexURL / NotifyPlexToken point at a Plex server refreshed via
	// per-folder partial scans.
	NotifyPlexURL   string
	NotifyPlexToken string
	// MountPath, when set, makes wisp self-mount the library there via the
	// embedded rclone VFS. Empty = serve HTTP only (mount it yourself).
	MountPath string
	// MountAllowOther exposes the mount to other users (needed when another
	// container reads the mount as a different UID).
	MountAllowOther bool
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// ReadChunkSize is the initial VFS read chunk in bytes; it doubles up to
	// ReadChunkSizeLimit. Smaller reduces debrid over-fetch on seeks (more
	// concurrent viewers per bandwidth); larger favors sequential throughput.
	ReadChunkSize int64
	// ReadChunkSizeLimit caps the chunk ramp in bytes.
	ReadChunkSizeLimit int64

	// ScheduleInterval is the fallback ceiling for the monitor loop; the
	// scheduler otherwise wakes near a monitored item's next known air/release.
	ScheduleInterval time.Duration
	// TMDBAPIKey enables home-media release gating via TMDB (v3 key or v4 token).
	TMDBAPIKey string
	// TMDBMarkets is the ordered list of ISO-3166-1 regions whose digital/
	// physical release dates gate movies (any market releasing makes it eligible).
	TMDBMarkets []string
}

// SelfMount reports whether wisp should mount the library itself.
func (c *Config) SelfMount() bool { return c.MountPath != "" }

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	c := &Config{
		AIOStreamsURL:        strings.TrimSpace(os.Getenv("WISP_AIOSTREAMS_URL")),
		AIOStreamsPassword:   os.Getenv("WISP_AIOSTREAMS_PASSWORD"),
		ListenAddr:           envOr("WISP_LISTEN_ADDR", ":8080"),
		DBPath:               envOr("WISP_DB_PATH", "/data/wisp.db"),
		SiloWebhookURL:       strings.TrimSpace(os.Getenv("WISP_SILO_WEBHOOK_URL")),
		NotifyArrWebhookURL:  strings.TrimSpace(os.Getenv("WISP_NOTIFY_ARR_WEBHOOK_URL")),
		NotifyJellyfinURL:    strings.TrimSpace(os.Getenv("WISP_NOTIFY_JELLYFIN_URL")),
		NotifyJellyfinAPIKey: strings.TrimSpace(os.Getenv("WISP_NOTIFY_JELLYFIN_API_KEY")),
		NotifyEmbyURL:        strings.TrimSpace(os.Getenv("WISP_NOTIFY_EMBY_URL")),
		NotifyEmbyAPIKey:     strings.TrimSpace(os.Getenv("WISP_NOTIFY_EMBY_API_KEY")),
		NotifyPlexURL:        strings.TrimSpace(os.Getenv("WISP_NOTIFY_PLEX_URL")),
		NotifyPlexToken:      strings.TrimSpace(os.Getenv("WISP_NOTIFY_PLEX_TOKEN")),
		MountPath:            strings.TrimSpace(os.Getenv("WISP_MOUNT_PATH")),
		MountAllowOther:      boolEnv("WISP_MOUNT_ALLOW_OTHER", true),
		LogLevel:             strings.ToLower(envOr("WISP_LOG_LEVEL", "info")),
		ReadChunkSize:        sizeEnv("WISP_READ_CHUNK_SIZE", 32<<20),
		ReadChunkSizeLimit:   sizeEnv("WISP_READ_CHUNK_SIZE_LIMIT", 512<<20),
		ScheduleInterval:     durationEnv("WISP_SCHEDULE_INTERVAL", 2*time.Hour),
		TMDBAPIKey:           strings.TrimSpace(os.Getenv("WISP_TMDB_API_KEY")),
		TMDBMarkets:          listEnv("WISP_TMDB_MARKETS", []string{"US", "CA", "GB", "AU", "DE", "FR", "IT", "ES", "JP", "IN"}),
	}
	if c.AIOStreamsURL == "" {
		return nil, fmt.Errorf("WISP_AIOSTREAMS_URL is required")
	}
	return c, nil
}

// durationEnv parses a Go duration like "2h" or "90m", falling back on empty or
// unparseable input. A non-positive duration also falls back.
func durationEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// listEnv parses a comma-separated list, upper-casing and trimming each entry
// (markets are ISO-3166-1 codes). Empty input yields the fallback.
func listEnv(key string, fallback []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.ToUpper(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// sizeEnv parses a byte size like "16M", "512M", "1G", or a plain byte count,
// falling back to the default on an empty or unparseable value.
func sizeEnv(key string, fallback int64) int64 {
	v := strings.TrimSpace(strings.ToUpper(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "G"):
		mult, v = 1<<30, strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		mult, v = 1<<20, strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		mult, v = 1<<10, strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n * mult
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

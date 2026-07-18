// Package config loads Wisp's runtime settings from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
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
	// MountPath, when set, makes wisp self-mount the library there via the
	// embedded rclone VFS. Empty = serve HTTP only (mount it yourself).
	MountPath string
	// MountAllowOther exposes the mount to other users (needed when another
	// container reads the mount as a different UID).
	MountAllowOther bool
}

// SelfMount reports whether wisp should mount the library itself.
func (c *Config) SelfMount() bool { return c.MountPath != "" }

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	c := &Config{
		AIOStreamsURL:      strings.TrimSpace(os.Getenv("WISP_AIOSTREAMS_URL")),
		AIOStreamsPassword: os.Getenv("WISP_AIOSTREAMS_PASSWORD"),
		ListenAddr:         envOr("WISP_LISTEN_ADDR", ":8080"),
		DBPath:             envOr("WISP_DB_PATH", "/data/wisp.db"),
		MountPath:          strings.TrimSpace(os.Getenv("WISP_MOUNT_PATH")),
		MountAllowOther:    boolEnv("WISP_MOUNT_ALLOW_OTHER", true),
	}
	if c.AIOStreamsURL == "" {
		return nil, fmt.Errorf("WISP_AIOSTREAMS_URL is required")
	}
	return c, nil
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

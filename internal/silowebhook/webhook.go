// Package silowebhook sends ARR-compatible Autoscan events to Silo.
package silowebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

type Client struct {
	url        string
	mountPath  string
	httpClient *http.Client
	log        *slog.Logger
}

func New(webhookURL, mountPath string, log *slog.Logger) *Client {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		mountPath = "/mnt/wisp"
	}
	return &Client{
		url: strings.TrimSpace(webhookURL), mountPath: mountPath,
		httpClient: &http.Client{Timeout: 10 * time.Second}, log: log,
	}
}

func (c *Client) Import(ctx context.Context, mediaType, virtualPath string) {
	fullPath := c.fullPath(virtualPath)
	payload := map[string]any{"eventType": "Download"}
	if mediaType == "series" {
		payload["episodeFile"] = map[string]string{"path": fullPath}
	} else {
		payload["movieFile"] = map[string]string{"path": fullPath}
	}
	c.send(ctx, "import", payload)
}

func (c *Client) Rename(ctx context.Context, mediaType, previousPath, newPath string) {
	entry := map[string]string{"previousPath": c.fullPath(previousPath), "newPath": c.fullPath(newPath)}
	payload := map[string]any{"eventType": "Rename"}
	if mediaType == "series" {
		payload["renamedEpisodeFiles"] = []map[string]string{entry}
	} else {
		payload["renamedMovieFiles"] = []map[string]string{entry}
	}
	c.send(ctx, "rename", payload)
}

func (c *Client) Delete(ctx context.Context, mediaType, virtualPath string) {
	fullPath := c.fullPath(virtualPath)
	file := map[string]string{"path": fullPath, "relativePath": path.Base(fullPath)}
	payload := map[string]any{"instanceName": "Wisp", "deleteReason": "Manual"}
	if mediaType == "series" {
		payload["eventType"] = "EpisodeFileDelete"
		payload["series"] = map[string]string{"path": path.Dir(path.Dir(fullPath))}
		payload["episodeFile"] = file
	} else {
		payload["eventType"] = "MovieFileDelete"
		payload["movie"] = map[string]string{"folderPath": path.Dir(fullPath)}
		payload["movieFile"] = file
	}
	c.send(ctx, "delete", payload)
}

func (c *Client) fullPath(virtualPath string) string {
	return path.Join(c.mountPath, strings.TrimLeft(virtualPath, "/"))
}

func (c *Client) send(ctx context.Context, event string, payload any) {
	if c == nil || c.url == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.log.Warn("Silo webhook encode failed", "event", event, "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.log.Warn("Silo webhook request failed", "event", event, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wisp")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Warn("Silo webhook delivery failed", "event", event, "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Warn("Silo webhook rejected", "event", event, "status", resp.StatusCode)
		return
	}
	c.log.Info("Silo webhook delivered", "event", event)
}

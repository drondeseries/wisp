package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

// arrTarget sends ARR-compatible Autoscan events (Sonarr/Radarr shape) to a
// single webhook URL — the format Silo's Autoscan sources accept natively.
type arrTarget struct {
	url        string
	mountPath  string
	httpClient *http.Client
	log        *slog.Logger
}

func newArrTarget(webhookURL, mountPath string, log *slog.Logger) *arrTarget {
	return &arrTarget{
		url:        strings.TrimSpace(webhookURL),
		mountPath:  mountPath,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

func (t *arrTarget) name() string { return "arr-webhook" }

func (t *arrTarget) Import(ctx context.Context, mediaType, virtualPath string) {
	full := fullPath(t.mountPath, virtualPath)
	payload := map[string]any{"eventType": "Download"}
	if mediaType == "series" {
		payload["episodeFile"] = map[string]string{"path": full}
	} else {
		payload["movieFile"] = map[string]string{"path": full}
	}
	t.send(ctx, "import", payload)
}

func (t *arrTarget) Rename(ctx context.Context, mediaType, previousPath, newPath string) {
	entry := map[string]string{"previousPath": fullPath(t.mountPath, previousPath), "newPath": fullPath(t.mountPath, newPath)}
	payload := map[string]any{"eventType": "Rename"}
	if mediaType == "series" {
		payload["renamedEpisodeFiles"] = []map[string]string{entry}
	} else {
		payload["renamedMovieFiles"] = []map[string]string{entry}
	}
	t.send(ctx, "rename", payload)
}

func (t *arrTarget) Delete(ctx context.Context, mediaType, virtualPath string) {
	full := fullPath(t.mountPath, virtualPath)
	file := map[string]string{"path": full, "relativePath": path.Base(full)}
	payload := map[string]any{"instanceName": "Wisp", "deleteReason": "Manual"}
	if mediaType == "series" {
		payload["eventType"] = "EpisodeFileDelete"
		payload["series"] = map[string]string{"path": path.Dir(path.Dir(full))}
		payload["episodeFile"] = file
	} else {
		payload["eventType"] = "MovieFileDelete"
		payload["movie"] = map[string]string{"folderPath": path.Dir(full)}
		payload["movieFile"] = file
	}
	t.send(ctx, "delete", payload)
}

func (t *arrTarget) send(ctx context.Context, event string, payload any) {
	if t == nil || t.url == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.log.Warn("arr webhook encode failed", "event", event, "error", err)
		return
	}
	status, err := postJSON(ctx, t.httpClient, t.url, nil, body)
	if err != nil {
		t.log.Warn("arr webhook delivery failed", "event", event, "error", err)
		return
	}
	if !okStatus(status) {
		t.log.Warn("arr webhook rejected", "event", event, "status", status)
		return
	}
	t.log.Info("arr webhook delivered", "event", event)
}

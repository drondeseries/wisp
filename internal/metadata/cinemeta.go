// Package metadata resolves provider ids (TVDB/TMDB) for an IMDb id so wisp can
// tag folders in a way media servers match deterministically.
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// cinemetaBase is the public Cinemeta endpoint; overridable in tests.
var cinemetaBase = "https://v3-cinemeta.strem.io"

type cinemetaMeta struct {
	Meta struct {
		TVDBID    json.Number `json:"tvdb_id"`
		MovieDBID json.Number `json:"moviedb_id"`
	} `json:"meta"`
}

// ProviderIDs looks up the TVDB and TMDB ids for an IMDb id via Cinemeta.
// Missing ids come back as "". Any error yields ("", "") — enrichment is
// best-effort and never blocks an add.
func ProviderIDs(ctx context.Context, mediaType, imdbID string) (tvdb, tmdb string) {
	kind := "movie"
	if mediaType == "series" {
		kind = "series"
	}
	endpoint := fmt.Sprintf("%s/meta/%s/%s.json", cinemetaBase, kind, imdbID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "wisp")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var meta cinemetaMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&meta); err != nil {
		return "", ""
	}
	return numString(meta.Meta.TVDBID), numString(meta.Meta.MovieDBID)
}

// numString renders a JSON number id as a plain string, "" if zero/empty.
func numString(n json.Number) string {
	s := n.String()
	if s == "" || s == "0" {
		return ""
	}
	return s
}

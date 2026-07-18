// Package aiostreams talks to an AIOStreams instance's REST API to select
// playable streams and resolve anime ID mappings. It reuses the exact
// auth-derivation and Search API contract validated in silo-plugin-aiostreams.
package aiostreams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const userAgent = "wisp"

// Client is a thin AIOStreams REST client.
type Client struct {
	addonURL   string
	basicCreds string // "uuid:password"
	http       *http.Client
}

// Stream is one playable result from the Search API.
type Stream struct {
	URL      string
	Filename string
}

// New builds a client from an AIOStreams manifest URL and password.
func New(addonURL, password string) *Client {
	return &Client{
		addonURL:   strings.TrimSpace(addonURL),
		basicCreds: deriveCredentials(addonURL, password),
		http:       &http.Client{Timeout: 60 * time.Second},
	}
}

// deriveCredentials mirrors the plugin: a "uuid:password" secret is used
// verbatim; otherwise the UUID/alias is recovered from the /stremio/{id}/...
// path and paired with the password.
func deriveCredentials(addonURL, password string) string {
	password = strings.TrimSpace(password)
	if strings.Contains(password, ":") {
		return password
	}
	parsed, err := url.Parse(strings.TrimSpace(addonURL))
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, segment := range segments {
		if segment == "stremio" && i+1 < len(segments) {
			id := segments[i+1]
			if id == "u" && i+2 < len(segments) { // alias form: /stremio/u/{alias}
				id = segments[i+2]
			}
			if password == "" {
				return id
			}
			return id + ":" + password
		}
	}
	return ""
}

type searchResult struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type searchResponse struct {
	Success bool `json:"success"`
	Data    *struct {
		Results []searchResult `json:"results"`
	} `json:"data"`
}

// Search returns playable streams (those with a URL) for a movie or episode,
// ordered by AIOStreams' own ranking. mediaType is "movie" or "series".
func (c *Client) Search(ctx context.Context, mediaType, imdbID string, season, episode int) ([]Stream, error) {
	origin, err := c.origin()
	if err != nil {
		return nil, err
	}
	id := imdbID
	if mediaType == "series" {
		id = fmt.Sprintf("%s:%d:%d", imdbID, season, episode)
	}
	q := url.Values{}
	q.Set("type", mediaType)
	q.Set("id", id)
	q.Set("requiredFields", "url")
	endpoint := origin + "/api/v1/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	c.applyAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned HTTP %d", resp.StatusCode)
	}
	var payload searchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	if !payload.Success || payload.Data == nil {
		return nil, fmt.Errorf("search response unsuccessful")
	}
	streams := make([]Stream, 0, len(payload.Data.Results))
	for _, r := range payload.Data.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		streams = append(streams, Stream{URL: r.URL, Filename: filenameFromResult(r)})
	}
	return streams, nil
}

func (c *Client) applyAuth(req *http.Request) {
	if parts := strings.SplitN(c.basicCreds, ":", 2); len(parts) == 2 {
		req.SetBasicAuth(parts[0], parts[1])
	}
}

func (c *Client) origin() (string, error) {
	parsed, err := url.Parse(c.addonURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid AIOStreams URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// filenameFromResult recovers the release filename (for extension hints)
// from the resolver URL's last path segment, falling back to the name field.
func filenameFromResult(r searchResult) string {
	if parsed, err := url.Parse(r.URL); err == nil {
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		for i := len(segments) - 1; i >= 0; i-- {
			if decoded, err := url.PathUnescape(segments[i]); err == nil {
				if strings.Contains(decoded, ".") {
					return decoded
				}
			}
		}
	}
	return strings.TrimSpace(r.Name)
}

package seerr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Seerr API to complete a request the webhook underspecifies
// (seasons, 4K intent, title/year). It authenticates with an API key.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a Seerr API client. baseURL/apiKey may be empty (enrichment then
// no-ops, and the webhook's own fields are used as-is).
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Configured reports whether the client can reach Seerr.
func (c *Client) Configured() bool { return c.baseURL != "" && c.apiKey != "" }

// Enrich fills gaps in an intake from the Seerr API — best effort. Webhook data
// is authoritative where present; the API only supplies what's missing (seasons,
// 4K flag, ids, and title/year for folder naming).
func (c *Client) Enrich(ctx context.Context, in *Intake) {
	if in == nil || !c.Configured() {
		return
	}
	if in.RequestID > 0 {
		if r, err := c.request(ctx, in.RequestID); err == nil {
			in.Is4K = r.Is4K // API is authoritative for 4K intent
			if in.TMDbID == "" {
				in.TMDbID = numToStr(r.Media.TMDbID)
			}
			if in.TVDbID == "" {
				in.TVDbID = numToStr(r.Media.TVDbID)
			}
			if in.IMDbID == "" {
				in.IMDbID = strings.TrimSpace(r.Media.IMDbID)
			}
			if len(in.Seasons) == 0 {
				for _, s := range r.Seasons {
					if s.SeasonNumber > 0 {
						in.Seasons = append(in.Seasons, s.SeasonNumber)
					}
				}
			}
		}
	}
	if (in.Title == "" || in.Year == 0) && in.TMDbID != "" {
		if title, year, err := c.mediaDetails(ctx, in.MediaType, in.TMDbID); err == nil {
			if in.Title == "" {
				in.Title = title
			}
			if in.Year == 0 {
				in.Year = year
			}
		}
	}
}

type seerrRequest struct {
	Is4K  bool `json:"is4k"`
	Media struct {
		TMDbID json.Number `json:"tmdbId"`
		TVDbID json.Number `json:"tvdbId"`
		IMDbID string      `json:"imdbId"`
	} `json:"media"`
	Seasons []struct {
		SeasonNumber int `json:"seasonNumber"`
	} `json:"seasons"`
}

func (c *Client) request(ctx context.Context, id int) (seerrRequest, error) {
	var r seerrRequest
	err := c.getJSON(ctx, c.baseURL+"/api/v1/request/"+strconv.Itoa(id), &r)
	return r, err
}

// mediaDetails fetches a title and year from Seerr's media endpoint.
func (c *Client) mediaDetails(ctx context.Context, mediaType, tmdbID string) (title string, year int, err error) {
	kind := "movie"
	if mediaType == "series" {
		kind = "tv"
	}
	var d struct {
		Title        string `json:"title"`        // movie
		Name         string `json:"name"`         // tv
		ReleaseDate  string `json:"releaseDate"`  // movie
		FirstAirDate string `json:"firstAirDate"` // tv
	}
	if err := c.getJSON(ctx, c.baseURL+"/api/v1/"+kind+"/"+tmdbID, &d); err != nil {
		return "", 0, err
	}
	title = d.Title
	if title == "" {
		title = d.Name
	}
	for _, date := range []string{d.ReleaseDate, d.FirstAirDate} {
		if len(date) >= 4 {
			if y, e := strconv.Atoi(date[:4]); e == nil {
				year = y
				break
			}
		}
	}
	return title, year, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("User-Agent", "wisp")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("seerr GET returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func numToStr(n json.Number) string {
	s := strings.TrimSpace(n.String())
	if s == "" || s == "0" {
		return ""
	}
	return s
}

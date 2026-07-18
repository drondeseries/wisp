package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNoHomeRelease means TMDB has release data for a movie but no digital or
// physical date — i.e. it is theatrical-only, so no stream should exist yet.
// This is distinct from "TMDB unavailable": the caller must NOT fall back to a
// theatrical date (which would pin a cam) when it sees this.
var ErrNoHomeRelease = errors.New("tmdb: no digital or physical release date")

// HasTMDB reports whether a TMDB key is configured.
func (s *Service) HasTMDB() bool { return s.tmdbKey != "" }

// tmdbAuth stamps TMDB credentials: a v4 token (two dots) goes in the
// Authorization header; a v3 key goes in the api_key query parameter.
func (s *Service) tmdbAuth(req *http.Request) {
	if strings.Count(s.tmdbKey, ".") == 2 {
		req.Header.Set("Authorization", "Bearer "+s.tmdbKey)
		return
	}
	q := req.URL.Query()
	q.Set("api_key", s.tmdbKey)
	req.URL.RawQuery = q.Encode()
}

type tmdbReleaseDates struct {
	Results []struct {
		Country string `json:"iso_3166_1"`
		Dates   []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		} `json:"release_dates"`
	} `json:"results"`
}

// MovieHomeRelease returns the earliest digital (type 4) or physical (type 5)
// release date for a movie across the configured markets. It returns
// ErrNoHomeRelease when TMDB responds but lists no home-media date.
func (s *Service) MovieHomeRelease(ctx context.Context, tmdbID string) (time.Time, error) {
	tmdbID = strings.TrimSpace(tmdbID)
	if tmdbID == "" || s.tmdbKey == "" {
		return time.Time{}, errors.New("tmdb: id and api key required")
	}
	endpoint := s.tmdbBase + "/movie/" + url.PathEscape(tmdbID) + "/release_dates"
	var payload tmdbReleaseDates
	if err := s.getJSON(ctx, endpoint, &payload, s.tmdbAuth); err != nil {
		return time.Time{}, err
	}
	if t, ok := selectHomeRelease(payload, s.tmdbMarkets); ok {
		return t, nil
	}
	return time.Time{}, ErrNoHomeRelease
}

// selectHomeRelease picks the earliest home-media date — digital (type 4) or
// physical (type 5), whichever comes first — across the configured markets.
// Types 1-3 are premiere/theatrical and type 6 is TV; none imply a home stream
// exists, so they never make a movie eligible.
func selectHomeRelease(payload tmdbReleaseDates, markets []string) (time.Time, bool) {
	want := make(map[string]bool, len(markets))
	for _, m := range markets {
		want[strings.ToUpper(strings.TrimSpace(m))] = true
	}
	var earliest time.Time
	for _, r := range payload.Results {
		if len(want) > 0 && !want[strings.ToUpper(r.Country)] {
			continue
		}
		for _, d := range r.Dates {
			if (d.Type == 4 || d.Type == 5) && !d.Date.IsZero() && (earliest.IsZero() || d.Date.Before(earliest)) {
				earliest = d.Date
			}
		}
	}
	return earliest, !earliest.IsZero()
}

// IMDbForTMDB resolves a TMDB id to its IMDb id via /external_ids. Requests may
// carry only a TMDB id, but Cinemeta (canonical episode numbering) is IMDb-keyed.
func (s *Service) IMDbForTMDB(ctx context.Context, mediaType, tmdbID string) (string, error) {
	tmdbID = strings.TrimSpace(tmdbID)
	if tmdbID == "" || s.tmdbKey == "" {
		return "", errors.New("tmdb: id and api key required")
	}
	kind := "movie"
	if mediaType == "series" || mediaType == "tv" {
		kind = "tv"
	}
	endpoint := s.tmdbBase + "/" + kind + "/" + url.PathEscape(tmdbID) + "/external_ids"
	var payload struct {
		IMDbID string `json:"imdb_id"`
	}
	if err := s.getJSON(ctx, endpoint, &payload, s.tmdbAuth); err != nil {
		return "", err
	}
	if id := strings.TrimSpace(payload.IMDbID); strings.HasPrefix(id, "tt") {
		return id, nil
	}
	return "", errors.New("tmdb: no imdb id for title")
}

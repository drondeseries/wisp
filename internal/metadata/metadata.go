// Package metadata resolves release intelligence for a title: provider ids for
// folder tags, home-media release dates for movies (so wisp doesn't pin a
// theatrical-only title), and the canonical episode set with air dates for
// series (so wisp only pins episodes that have actually aired).
//
// Sources: Cinemeta (canonical Stremio season/episode numbering, which is what
// AIOStreams resolves against), TVmaze (precise per-episode airstamps), and
// TMDB (digital/physical movie release dates and tmdb->imdb resolution).
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const userAgent = "wisp"

// Episode is one episode in a series' canonical numbering. Aired is zero when
// no provider supplied an air date (treated as not-yet-released).
type Episode struct {
	Season int
	Number int
	Aired  time.Time
}

// Service fetches release metadata. It is safe for concurrent use.
type Service struct {
	http        *http.Client
	tmdbKey     string
	tmdbMarkets []string

	// Base URLs, overridable in tests.
	cinemetaBase string
	tmdbBase     string
	tvmazeBase   string
}

// Option configures a Service.
type Option func(*Service)

// WithBaseURLs overrides the provider base URLs (used by tests).
func WithBaseURLs(cinemeta, tmdb, tvmaze string) Option {
	return func(s *Service) { s.cinemetaBase, s.tmdbBase, s.tvmazeBase = cinemeta, tmdb, tvmaze }
}

// New builds a metadata service. tmdbKey may be empty (movie gating then falls
// back to Cinemeta); markets defaults to US when empty.
func New(tmdbKey string, markets []string, opts ...Option) *Service {
	if len(markets) == 0 {
		markets = []string{"US"}
	}
	s := &Service{
		http:         &http.Client{Timeout: 25 * time.Second},
		tmdbKey:      strings.TrimSpace(tmdbKey),
		tmdbMarkets:  markets,
		cinemetaBase: "https://v3-cinemeta.strem.io",
		tmdbBase:     "https://api.themoviedb.org/3",
		tvmazeBase:   "https://api.tvmaze.com",
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// getJSON performs a GET and decodes a JSON body into out. auth, when set,
// stamps credentials onto the request. Error messages carry only the path (no
// query) so an api_key in the query string is never logged.
func (s *Service) getJSON(ctx context.Context, endpoint string, out any, auth func(*http.Request)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	if auth != nil {
		auth(req)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", pathOnly(endpoint), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", pathOnly(endpoint), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", pathOnly(endpoint), err)
	}
	return nil
}

// pathOnly strips the query and fragment from a URL for safe logging.
func pathOnly(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

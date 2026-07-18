// Package seerr ingests Overseerr/Jellyseerr request webhooks and, when needed,
// queries the Seerr API to complete a request (seasons, 4K intent). It is the
// push-first front door: Seerr tells wisp what to pin; wisp never polls Seerr.
package seerr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Intake is a normalized, actionable request derived from a webhook (and
// optionally enriched from the Seerr API).
type Intake struct {
	MediaType string // "movie" | "series"
	TMDbID    string
	TVDbID    string
	IMDbID    string
	Title     string
	Year      int
	Seasons   []int // requested seasons; empty = all/unspecified
	Is4K      bool
	RequestID int // Seerr request id, for API enrichment (0 if unknown)
}

// webhookPayload mirrors the fields of Overseerr/Jellyseerr's default JSON
// notification that wisp needs. Ids arrive as numbers or strings across
// versions, so they are decoded leniently.
type webhookPayload struct {
	NotificationType string `json:"notification_type"`
	Subject          string `json:"subject"`
	Media            struct {
		MediaType string      `json:"media_type"`
		TMDbID    json.Number `json:"tmdbId"`
		TVDbID    json.Number `json:"tvdbId"`
		IMDbID    string      `json:"imdbId"`
	} `json:"media"`
	Request struct {
		RequestID json.Number `json:"request_id"`
		Is4K      *bool       `json:"is4k"`
	} `json:"request"`
	Extra []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"extra"`
}

// ParseWebhook decodes a Seerr webhook body. actionable is false (with nil
// error) for events wisp ignores — test pings and non-approval notifications —
// so the caller can just return 200.
func ParseWebhook(body []byte) (in *Intake, actionable bool, err error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false, fmt.Errorf("decode webhook: %w", err)
	}
	switch strings.ToUpper(strings.TrimSpace(p.NotificationType)) {
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
		// act on it
	default:
		return nil, false, nil // TEST_NOTIFICATION, MEDIA_PENDING, MEDIA_AVAILABLE, …
	}

	mediaType := "movie"
	if mt := strings.ToLower(strings.TrimSpace(p.Media.MediaType)); mt == "tv" || mt == "series" {
		mediaType = "series"
	}
	intake := &Intake{
		MediaType: mediaType,
		TMDbID:    numStr(p.Media.TMDbID),
		TVDbID:    numStr(p.Media.TVDbID),
		IMDbID:    strings.TrimSpace(p.Media.IMDbID),
		Title:     titleFromSubject(p.Subject),
		Year:      yearFromSubject(p.Subject),
		Seasons:   parseSeasons(p.Extra),
		RequestID: atoiSafe(numStr(p.Request.RequestID)),
	}
	// The default webhook template exposes no reliable per-request 4K flag
	// (media.status4k is the title's overall 4K status, not this request's), so
	// is4k is trusted only from a custom template's request.is4k here; the Seerr
	// API (Client.Enrich) is the authoritative source and overrides this.
	if p.Request.Is4K != nil {
		intake.Is4K = *p.Request.Is4K
	}
	if intake.TMDbID == "" && intake.IMDbID == "" {
		return nil, false, fmt.Errorf("webhook carries no tmdb or imdb id")
	}
	return intake, true, nil
}

// Qualities maps Seerr's HD/4K intent to wisp quality tiers, so a standard and a
// 4K request of the same title become two distinct files. A single tier is
// returned; an empty result never happens (standard defaults to 1080p).
func (in *Intake) Qualities() []string {
	if in.Is4K {
		return []string{"2160p"}
	}
	return []string{"1080p"}
}

func parseSeasons(extra []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}) []int {
	var seasons []int
	for _, e := range extra {
		if !strings.Contains(strings.ToLower(e.Name), "season") {
			continue
		}
		for _, tok := range strings.FieldsFunc(e.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
			if n, err := strconv.Atoi(strings.TrimSpace(tok)); err == nil && n > 0 {
				seasons = append(seasons, n)
			}
		}
	}
	return seasons
}

// titleFromSubject extracts the title from a subject like "Inception (2010)".
func titleFromSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if i := strings.LastIndex(subject, "("); i > 0 {
		return strings.TrimSpace(subject[:i])
	}
	return subject
}

// yearFromSubject extracts a 4-digit year from a subject like "Inception (2010)".
func yearFromSubject(subject string) int {
	open := strings.LastIndex(subject, "(")
	end := strings.LastIndex(subject, ")")
	if open >= 0 && end > open+1 {
		if n, err := strconv.Atoi(strings.TrimSpace(subject[open+1 : end])); err == nil && n > 1800 && n < 3000 {
			return n
		}
	}
	return 0
}

func numStr(n json.Number) string {
	s := strings.TrimSpace(n.String())
	if s == "" || s == "0" {
		return ""
	}
	return s
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

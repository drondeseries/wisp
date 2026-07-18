package metadata

import (
	"context"
	"net/url"
	"strings"
)

// AnimeHeuristic guesses whether a title is anime from the Cinemeta metadata
// wisp already has access to. It is intentionally minimal and conservative:
//
//   - Requires the "Animation" genre, AND
//   - Requires a positive Japanese signal (original language "ja"/"japanese"
//     or country "jp"/"japan"), when Cinemeta supplies one.
//
// Requiring the Japanese signal keeps Western animation (Pixar, DreamWorks) out
// of the anime roots. When signals are unavailable — the lookup fails, the genre
// is absent, or no language/country is present to confirm Japanese — it returns
// false (default non-anime). It is best-effort and never blocks intake.
//
// The category this feeds is decided ONCE per title and stored, so a later
// metadata change can never move an already-categorized title.
func (s *Service) AnimeHeuristic(ctx context.Context, mediaType, imdbID string) bool {
	imdbID = strings.TrimSpace(imdbID)
	if !strings.HasPrefix(imdbID, "tt") {
		return false // Cinemeta is imdb-keyed; nothing to look up
	}
	kind := "movie"
	if mediaType == "series" {
		kind = "series"
	}
	var payload struct {
		Meta struct {
			Genres   []string `json:"genres"`
			Country  string   `json:"country"`
			Language string   `json:"language"`
		} `json:"meta"`
	}
	endpoint := s.cinemetaBase + "/meta/" + kind + "/" + url.PathEscape(imdbID) + ".json"
	if err := s.getJSON(ctx, endpoint, &payload, nil); err != nil {
		return false
	}
	animation := false
	for _, g := range payload.Meta.Genres {
		if strings.EqualFold(strings.TrimSpace(g), "Animation") {
			animation = true
			break
		}
	}
	if !animation {
		return false
	}
	return isJapanese(payload.Meta.Language) || isJapanese(payload.Meta.Country)
}

// isJapanese reports whether a Cinemeta language or country field names Japan.
func isJapanese(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "ja", "jp", "jpn", "japanese", "japan":
		return true
	}
	return strings.Contains(s, "japan")
}

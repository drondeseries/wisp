// Package library builds the virtual directory layout that media servers scan.
package library

import (
	"fmt"
	"path"
	"strings"
)

// IDs carries provider identifiers used to tag folder names, so scanners match
// a title deterministically by ID instead of guessing from the title text.
// Silo/Plex/Jellyfin all read tags like "[tvdb-439755]" / "[tmdb-27205]" /
// "[imdb-tt1375666]" from the parent folder.
type IDs struct {
	IMDb string // "tt…" (a "tmdb:…" value is also accepted and emitted as tmdb)
	TMDb string
	TVDb string
}

// tag returns a folder-name suffix such as " [tvdb-439755]" using the most
// authoritative id available for the media type (TVDB for series, TMDB for
// movies, IMDb as the universal fallback), or "" if none is present.
func (ids IDs) tag(mediaType string) string {
	if mediaType == "series" {
		if v := sanitizeID(ids.TVDb); v != "" {
			return " [tvdb-" + v + "]"
		}
	}
	if v := sanitizeID(ids.TMDb); v != "" {
		return " [tmdb-" + v + "]"
	}
	imdb := strings.TrimSpace(ids.IMDb)
	if rest, ok := strings.CutPrefix(imdb, "tmdb:"); ok {
		if v := sanitizeID(rest); v != "" {
			return " [tmdb-" + v + "]"
		}
	}
	if strings.HasPrefix(imdb, "tt") {
		if v := sanitizeID(imdb); v != "" {
			return " [imdb-" + v + "]"
		}
	}
	return ""
}

// MoviePath returns the virtual path for a movie shortcut, mirroring the
// naming a Silo/Plex/Jellyfin scanner expects, with a provider-id folder tag.
func MoviePath(title string, year int, ids IDs, quality, ext string) string {
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year) + ids.tag("movie")
	file := fmt.Sprintf("%s (%d) - [%s]%s", sanitize(title), year, quality, ext)
	return path.Join("movies", folder, file)
}

// EpisodePath returns the virtual path for a series episode shortcut. The
// provider-id tag lives on the series folder (not the season folder).
func EpisodePath(title string, year, season, episode int, ids IDs, quality, ext string) string {
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year) + ids.tag("series")
	seasonDir := fmt.Sprintf("Season %02d", season)
	file := fmt.Sprintf("%s (%d) - S%02dE%02d - [%s]%s", sanitize(title), year, season, episode, quality, ext)
	return path.Join("shows", folder, seasonDir, file)
}

// sanitizeID keeps only the word characters Silo's folder-id pattern accepts.
func sanitizeID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DetectQuality reads a resolution label from a release filename or title,
// returning "" when none is recognizable. This lets the label reflect the
// stream AIOStreams actually selected rather than a caller's guess.
func DetectQuality(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "2160p"), strings.Contains(s, "4k"), strings.Contains(s, "uhd"):
		return "2160p"
	case strings.Contains(s, "1080p"):
		return "1080p"
	case strings.Contains(s, "720p"):
		return "720p"
	case strings.Contains(s, "480p"):
		return "480p"
	default:
		return ""
	}
}

// Ext picks a media extension from a release filename, defaulting to .mkv.
func Ext(filename string) string {
	filename = strings.ToLower(strings.TrimSpace(filename))
	for _, e := range []string{".mkv", ".mp4", ".m4v", ".avi", ".ts", ".wmv"} {
		if strings.HasSuffix(filename, e) {
			return e
		}
	}
	return ".mkv"
}

// sanitize strips characters that break filesystem paths across platforms.
func sanitize(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -", "*", "", "?", "",
		"\"", "'", "<", "", ">", "", "|", "", "\n", " ", "\t", " ",
	)
	return strings.TrimSpace(replacer.Replace(name))
}

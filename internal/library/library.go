// Package library builds the virtual directory layout that media servers scan.
package library

import (
	"fmt"
	"path"
	"strings"
)

// The four library roots wisp presents, one per Silo library. A title's root is
// fixed by its category (media type × is-anime) at first intake and is part of
// every VirtualPath (the bbolt key and on-disk location), so it must never be
// re-derived for an existing title — see internal/monitor for the invariant.
const (
	RootMovies      = "movies"
	RootShows       = "shows"
	RootAnimeMovies = "anime_movies"
	RootAnimeShows  = "anime_shows"
)

// Roots returns the four library roots wisp always presents (even when empty),
// so a media server can validate all four library paths from an empty wisp.
func Roots() []string {
	return []string{RootAnimeMovies, RootAnimeShows, RootMovies, RootShows}
}

// Root maps a category (media type × is-anime) to the library root that holds
// it. This is the single source of truth the path builder and the always-present
// directory listing share.
func Root(mediaType string, isAnime bool) string {
	series := mediaType == "series"
	switch {
	case series && isAnime:
		return RootAnimeShows
	case series:
		return RootShows
	case isAnime:
		return RootAnimeMovies
	default:
		return RootMovies
	}
}

// IsRoot reports whether name is one of the four library roots.
func IsRoot(name string) bool {
	switch name {
	case RootMovies, RootShows, RootAnimeMovies, RootAnimeShows:
		return true
	}
	return false
}

// RootOf returns the library root a virtual path lives under, or "" when the
// path is not under a known root. Used to backfill a pin's category from its
// existing VirtualPath prefix without rewriting the path.
func RootOf(virtualPath string) string {
	vp := strings.TrimLeft(virtualPath, "/")
	if i := strings.IndexByte(vp, '/'); i >= 0 {
		vp = vp[:i]
	}
	if IsRoot(vp) {
		return vp
	}
	return ""
}

// MediaTypeForRoot returns "series" for the two show roots and "movie"
// otherwise, so callers that only have a path/root can classify it.
func MediaTypeForRoot(root string) string {
	if root == RootShows || root == RootAnimeShows {
		return "series"
	}
	return "movie"
}

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
// root is the movie library root the title's category resolved to (RootMovies
// or RootAnimeMovies); an empty root falls back to RootMovies.
func MoviePath(root, title string, year int, ids IDs, quality, ext string) string {
	if root == "" {
		root = RootMovies
	}
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year) + ids.tag("movie")
	file := fmt.Sprintf("%s (%d) - [%s]%s", sanitize(title), year, quality, ext)
	return path.Join(root, folder, file)
}

// EpisodePath returns the virtual path for a series episode shortcut. The
// provider-id tag lives on the series folder (not the season folder). root is
// the series library root the title's category resolved to (RootShows or
// RootAnimeShows); an empty root falls back to RootShows.
func EpisodePath(root, title string, year, season, episode int, ids IDs, quality, ext string) string {
	if root == "" {
		root = RootShows
	}
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year) + ids.tag("series")
	seasonDir := fmt.Sprintf("Season %02d", season)
	file := fmt.Sprintf("%s (%d) - S%02dE%02d - [%s]%s", sanitize(title), year, season, episode, quality, ext)
	return path.Join(root, folder, seasonDir, file)
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

// NormalizeQuality canonicalizes a requested quality label ("4k" → "2160p",
// "1080P" → "1080p") to the same vocabulary DetectQuality emits, so a request
// quality and a parsed release resolution compare equal. Returns "" when the
// label is unrecognized (treated as "no quality constraint").
func NormalizeQuality(s string) string { return DetectQuality(s) }

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

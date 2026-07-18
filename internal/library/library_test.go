package library

import (
	"reflect"
	"sort"
	"testing"
)

func TestMoviePath(t *testing.T) {
	got := MoviePath(RootMovies, "Inception", 2010, IDs{}, "2160p", ".mkv")
	want := "movies/Inception (2010)/Inception (2010) - [2160p].mkv"
	if got != want {
		t.Fatalf("MoviePath() = %q, want %q", got, want)
	}
}

func TestEpisodePath(t *testing.T) {
	got := EpisodePath(RootShows, "The Villager of Level 999", 2026, 1, 4, IDs{}, "1080p", ".mkv")
	want := "shows/The Villager of Level 999 (2026)/Season 01/The Villager of Level 999 (2026) - S01E04 - [1080p].mkv"
	if got != want {
		t.Fatalf("EpisodePath() = %q, want %q", got, want)
	}
}

func TestEpisodePathZeroPads(t *testing.T) {
	got := EpisodePath(RootShows, "Show", 2020, 12, 105, IDs{}, "1080p", ".mp4")
	want := "shows/Show (2020)/Season 12/Show (2020) - S12E105 - [1080p].mp4"
	if got != want {
		t.Fatalf("EpisodePath() = %q, want %q", got, want)
	}
}

// The path builder routes each category to its own root; the rest of the path
// (folder/season/file naming) is identical across roots.
func TestCategoryRoots(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		isAnime   bool
		wantRoot  string
		got       string
	}{
		{"movie", "movie", false, "movies",
			MoviePath(Root("movie", false), "Inception", 2010, IDs{}, "2160p", ".mkv")},
		{"anime movie", "movie", true, "anime_movies",
			MoviePath(Root("movie", true), "Akira", 1988, IDs{}, "2160p", ".mkv")},
		{"series", "series", false, "shows",
			EpisodePath(Root("series", false), "Show", 2020, 1, 1, IDs{}, "1080p", ".mkv")},
		{"anime series", "series", true, "anime_shows",
			EpisodePath(Root("series", true), "Frieren", 2023, 1, 1, IDs{}, "1080p", ".mkv")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if Root(tc.mediaType, tc.isAnime) != tc.wantRoot {
				t.Fatalf("Root(%q,%v) = %q, want %q", tc.mediaType, tc.isAnime, Root(tc.mediaType, tc.isAnime), tc.wantRoot)
			}
			if RootOf(tc.got) != tc.wantRoot {
				t.Fatalf("built path %q not under root %q", tc.got, tc.wantRoot)
			}
		})
	}
}

func TestRoots(t *testing.T) {
	got := append([]string(nil), Roots()...)
	sort.Strings(got)
	want := []string{"anime_movies", "anime_shows", "movies", "shows"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}
	for _, r := range want {
		if !IsRoot(r) {
			t.Fatalf("IsRoot(%q) = false", r)
		}
	}
	if IsRoot("music") {
		t.Fatal("IsRoot(music) = true, want false")
	}
}

func TestRootOfAndMediaType(t *testing.T) {
	cases := map[string]struct {
		root      string
		mediaType string
	}{
		"movies/Film (2020)/f.mkv":             {"movies", "movie"},
		"/shows/Show (2020)/S01/e.mkv":         {"shows", "series"},
		"anime_movies/Akira (1988)/a.mkv":      {"anime_movies", "movie"},
		"anime_shows/Frieren (2023)/S01/e.mkv": {"anime_shows", "series"},
		"music/song.mp3":                       {"", "movie"}, // unknown root → "" (and default movie)
	}
	for path, want := range cases {
		if got := RootOf(path); got != want.root {
			t.Fatalf("RootOf(%q) = %q, want %q", path, got, want.root)
		}
		if got := MediaTypeForRoot(RootOf(path)); got != want.mediaType {
			t.Fatalf("MediaTypeForRoot(RootOf(%q)) = %q, want %q", path, got, want.mediaType)
		}
	}
}

// An empty root falls back to the non-anime default so a mis-set caller never
// produces a rootless path.
func TestEmptyRootFallsBack(t *testing.T) {
	if got := MoviePath("", "M", 2020, IDs{}, "1080p", ".mkv"); RootOf(got) != "movies" {
		t.Fatalf("empty movie root = %q", got)
	}
	if got := EpisodePath("", "S", 2020, 1, 1, IDs{}, "1080p", ".mkv"); RootOf(got) != "shows" {
		t.Fatalf("empty series root = %q", got)
	}
}

func TestExt(t *testing.T) {
	cases := map[string]string{
		"Show.S01E04.1080p.WEB.h264-GRP.mkv": ".mkv",
		"movie.2160p.mp4":                    ".mp4",
		"weird.release.no.ext":               ".mkv",
		"clip.TS":                            ".ts",
		"":                                   ".mkv",
	}
	for in, want := range cases {
		if got := Ext(in); got != want {
			t.Fatalf("Ext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeStripsPathBreakers(t *testing.T) {
	got := MoviePath(RootMovies, "Face/Off: Reboot?", 2027, IDs{}, "1080p", ".mkv")
	want := "movies/Face-Off - Reboot (2027)/Face-Off - Reboot (2027) - [1080p].mkv"
	if got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

func TestDetectQuality(t *testing.T) {
	cases := map[string]string{
		"Show.S01E04.2160p.WEB.mkv":      "2160p",
		"movie.4K.UHD.BluRay.mkv":        "2160p",
		"Show.S01E04.1080p.WEB.h264.mkv": "1080p",
		"old.720p.rip.mp4":               "720p",
		"unknown.release.mkv":            "",
	}
	for in, want := range cases {
		if got := DetectQuality(in); got != want {
			t.Fatalf("DetectQuality(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIDTags(t *testing.T) {
	// series prefers tvdb
	if got := EpisodePath(RootShows, "Show", 2026, 1, 4, IDs{IMDb: "tt9", TMDb: "500", TVDb: "439755"}, "1080p", ".mkv"); got != "shows/Show (2026) [tvdb-439755]/Season 01/Show (2026) - S01E04 - [1080p].mkv" {
		t.Fatalf("series tvdb tag: %q", got)
	}
	// movie prefers tmdb
	if got := MoviePath(RootMovies, "Inception", 2010, IDs{IMDb: "tt1375666", TMDb: "27205"}, "2160p", ".mkv"); got != "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [2160p].mkv" {
		t.Fatalf("movie tmdb tag: %q", got)
	}
	// imdb fallback when only imdb present
	if got := MoviePath(RootMovies, "Obscure", 2026, IDs{IMDb: "tt38262097"}, "1080p", ".mkv"); got != "movies/Obscure (2026) [imdb-tt38262097]/Obscure (2026) - [1080p].mkv" {
		t.Fatalf("imdb fallback: %q", got)
	}
	// tmdb: prefix in imdb field emits a tmdb tag
	if got := (IDs{IMDb: "tmdb:550"}).tag("movie"); got != " [tmdb-550]" {
		t.Fatalf("tmdb-prefixed imdb: %q", got)
	}
	// no ids -> no tag
	if got := (IDs{}).tag("series"); got != "" {
		t.Fatalf("empty tag: %q", got)
	}
}

func TestNormalizeQuality(t *testing.T) {
	cases := map[string]string{
		"2160p": "2160p", "4K": "2160p", "uhd": "2160p",
		"1080p": "1080p", "1080P": "1080p",
		"720p": "720p", "": "", "garbage": "",
	}
	for in, want := range cases {
		if got := NormalizeQuality(in); got != want {
			t.Fatalf("NormalizeQuality(%q) = %q, want %q", in, got, want)
		}
	}
}

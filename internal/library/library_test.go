package library

import "testing"

func TestMoviePath(t *testing.T) {
	got := MoviePath("Inception", 2010, IDs{}, "2160p", ".mkv")
	want := "movies/Inception (2010)/Inception (2010) - [2160p].mkv"
	if got != want {
		t.Fatalf("MoviePath() = %q, want %q", got, want)
	}
}

func TestEpisodePath(t *testing.T) {
	got := EpisodePath("The Villager of Level 999", 2026, 1, 4, IDs{}, "1080p", ".mkv")
	want := "shows/The Villager of Level 999 (2026)/Season 01/The Villager of Level 999 (2026) - S01E04 - [1080p].mkv"
	if got != want {
		t.Fatalf("EpisodePath() = %q, want %q", got, want)
	}
}

func TestEpisodePathZeroPads(t *testing.T) {
	got := EpisodePath("Show", 2020, 12, 105, IDs{}, "1080p", ".mp4")
	want := "shows/Show (2020)/Season 12/Show (2020) - S12E105 - [1080p].mp4"
	if got != want {
		t.Fatalf("EpisodePath() = %q, want %q", got, want)
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
	got := MoviePath("Face/Off: Reboot?", 2027, IDs{}, "1080p", ".mkv")
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
	if got := EpisodePath("Show", 2026, 1, 4, IDs{IMDb: "tt9", TMDb: "500", TVDb: "439755"}, "1080p", ".mkv"); got != "shows/Show (2026) [tvdb-439755]/Season 01/Show (2026) - S01E04 - [1080p].mkv" {
		t.Fatalf("series tvdb tag: %q", got)
	}
	// movie prefers tmdb
	if got := MoviePath("Inception", 2010, IDs{IMDb: "tt1375666", TMDb: "27205"}, "2160p", ".mkv"); got != "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [2160p].mkv" {
		t.Fatalf("movie tmdb tag: %q", got)
	}
	// imdb fallback when only imdb present
	if got := MoviePath("Obscure", 2026, IDs{IMDb: "tt38262097"}, "1080p", ".mkv"); got != "movies/Obscure (2026) [imdb-tt38262097]/Obscure (2026) - [1080p].mkv" {
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

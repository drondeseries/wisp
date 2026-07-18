package library

import "testing"

func TestMoviePath(t *testing.T) {
	got := MoviePath("Inception", 2010, "2160p", ".mkv")
	want := "movies/Inception (2010)/Inception (2010) - [2160p].mkv"
	if got != want {
		t.Fatalf("MoviePath() = %q, want %q", got, want)
	}
}

func TestEpisodePath(t *testing.T) {
	got := EpisodePath("The Villager of Level 999", 2026, 1, 4, "1080p", ".mkv")
	want := "shows/The Villager of Level 999 (2026)/Season 01/The Villager of Level 999 (2026) - S01E04 - [1080p].mkv"
	if got != want {
		t.Fatalf("EpisodePath() = %q, want %q", got, want)
	}
}

func TestEpisodePathZeroPads(t *testing.T) {
	got := EpisodePath("Show", 2020, 12, 105, "1080p", ".mp4")
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
	got := MoviePath("Face/Off: Reboot?", 2027, "1080p", ".mkv")
	want := "movies/Face-Off - Reboot (2027)/Face-Off - Reboot (2027) - [1080p].mkv"
	if got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

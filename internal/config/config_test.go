package config

import (
	"testing"
	"time"
)

func TestSizeEnv(t *testing.T) {
	cases := map[string]int64{
		"16M":     16 << 20,
		"512M":    512 << 20,
		"1G":      1 << 30,
		"8k":      8 << 10,
		"1048576": 1 << 20,
		"":        99, // fallback
		"bogus":   99, // fallback
		"0M":      99, // non-positive → fallback
	}
	for in, want := range cases {
		t.Setenv("WISP_TEST_SIZE", in)
		if got := sizeEnv("WISP_TEST_SIZE", 99); got != want {
			t.Fatalf("sizeEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBoolEnv(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "1": true, "on": true, "false": false, "0": false, "": true} {
		t.Setenv("WISP_TEST_BOOL", in)
		if got := boolEnv("WISP_TEST_BOOL", true); got != want {
			t.Fatalf("boolEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDurationEnv(t *testing.T) {
	t.Setenv("WISP_TEST_DUR", "90m")
	if got := durationEnv("WISP_TEST_DUR", time.Hour); got != 90*time.Minute {
		t.Fatalf("dur = %s", got)
	}
	t.Setenv("WISP_TEST_DUR", "garbage")
	if got := durationEnv("WISP_TEST_DUR", 2*time.Hour); got != 2*time.Hour {
		t.Fatalf("fallback = %s", got)
	}
}

func TestIntEnv(t *testing.T) {
	cases := map[string]int{
		"8":     8,
		"0":     0,
		"-3":    -3,
		"":      7, // fallback
		"bogus": 7, // fallback
		"1.5":   7, // fallback (not an int)
	}
	for in, want := range cases {
		t.Setenv("WISP_TEST_INT", in)
		if got := intEnv("WISP_TEST_INT", 7); got != want {
			t.Fatalf("intEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestClampInt(t *testing.T) {
	cases := []struct {
		n, lo, hi, want int
	}{
		{5, 1, 16, 5},   // in range
		{0, 1, 16, 1},   // below floor
		{-4, 1, 16, 1},  // below floor
		{99, 1, 16, 16}, // above ceiling
		{16, 1, 16, 16}, // at ceiling
		{1, 1, 16, 1},   // at floor
	}
	for _, c := range cases {
		if got := clampInt(c.n, c.lo, c.hi); got != c.want {
			t.Fatalf("clampInt(%d, %d, %d) = %d, want %d", c.n, c.lo, c.hi, got, c.want)
		}
	}
}

func TestListEnv(t *testing.T) {
	t.Setenv("WISP_TEST_LIST", " us , gb ,, jp ")
	got := listEnv("WISP_TEST_LIST", []string{"X"})
	if len(got) != 3 || got[0] != "US" || got[2] != "JP" {
		t.Fatalf("list = %v", got)
	}
	t.Setenv("WISP_TEST_LIST", "")
	if got := listEnv("WISP_TEST_LIST", []string{"X"}); len(got) != 1 || got[0] != "X" {
		t.Fatalf("fallback = %v", got)
	}
}

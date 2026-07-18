package config

import "testing"

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

package main

import (
	"strings"
	"testing"
)

// A representative /proc/self/mountinfo from a container whose /data is a bind
// volume but whose root is an overlay.
const mountinfoFixture = `22 28 0:21 / /sys rw,nosuid - sysfs sysfs rw
23 28 0:22 / /proc rw,nosuid - proc proc rw
24 28 0:5 / /dev rw,nosuid - devtmpfs devtmpfs rw
30 28 0:24 / / rw,relatime - overlay overlay rw,lowerdir=/x,upperdir=/y
100 30 259:1 /volumes/wisp /data rw,relatime - ext4 /dev/nvme0n1p1 rw
101 30 0:44 / /tmp rw,relatime - tmpfs tmpfs rw
102 30 259:1 /volumes/media /mnt/wisp/movies rw,relatime - ext4 /dev/nvme0n1p1 rw`

func TestParseMountinfo(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(mountinfoFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 7 {
		t.Fatalf("mounts = %d, want 7", len(mounts))
	}
	root := mounts[3]
	if root.mountPoint != "/" || root.fsType != "overlay" {
		t.Fatalf("root = %#v", root)
	}
	data := mounts[4]
	if data.mountPoint != "/data" || data.fsType != "ext4" {
		t.Fatalf("data = %#v", data)
	}
}

func TestParseMountinfoUnescapesSpaces(t *testing.T) {
	line := `100 30 259:1 / /mnt/my\040volume rw - ext4 /dev/sda1 rw`
	mounts, err := parseMountinfo(strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].mountPoint != "/mnt/my volume" {
		t.Fatalf("mount = %#v", mounts)
	}
}

func TestParseMountinfoSkipsMalformed(t *testing.T) {
	body := "short line\n" +
		"30 28 0:24 / / rw,relatime - overlay overlay rw\n" +
		"40 30 0:1 / /nope rw no-separator here\n"
	mounts, err := parseMountinfo(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].mountPoint != "/" {
		t.Fatalf("mounts = %#v", mounts)
	}
}

func TestLongestMountAndEphemeral(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(mountinfoFixture))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		dir           string
		wantMount     string
		wantEphemeral bool
	}{
		{"db on bind volume", "/data", "/data", false},
		{"db dir under bind volume", "/data/sub", "/data", false},
		{"db on overlay root", "/", "/", true},
		{"db under overlay root", "/config", "/", true},
		{"db on tmpfs", "/tmp/wisp", "/tmp", true},
		{"boundary not matched", "/database", "/", true}, // /data must not cover /database
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := longestMount(mounts, tc.dir)
			if !ok {
				t.Fatalf("no covering mount for %q", tc.dir)
			}
			if m.mountPoint != tc.wantMount {
				t.Fatalf("mount for %q = %q, want %q", tc.dir, m.mountPoint, tc.wantMount)
			}
			if got := isEphemeralMount(m); got != tc.wantEphemeral {
				t.Fatalf("isEphemeralMount for %q = %v, want %v", tc.dir, got, tc.wantEphemeral)
			}
		})
	}
}

// A file-level bind mount (-v /host/wisp.db:/data/wisp.db) on top of an overlay
// root must resolve to the file's own mount, not the overlay — no false warning.
func TestLongestMountFileBindMount(t *testing.T) {
	fixture := `30 28 0:24 / / rw,relatime - overlay overlay rw,lowerdir=/x
101 30 0:44 / /tmp rw,relatime - tmpfs tmpfs rw
200 30 259:1 /host/wisp.db /data/wisp.db rw,relatime - ext4 /dev/nvme0n1p1 rw`
	mounts, err := parseMountinfo(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	// The DB file path resolves to its own bind mount (ext4) — persistent.
	m, ok := longestMount(mounts, "/data/wisp.db")
	if !ok {
		t.Fatal("no covering mount for /data/wisp.db")
	}
	if m.mountPoint != "/data/wisp.db" || m.fsType != "ext4" {
		t.Fatalf("file bind mount = %#v, want /data/wisp.db ext4", m)
	}
	if isEphemeralMount(m) {
		t.Fatal("file bind mount wrongly flagged ephemeral")
	}
	// Guard the regression: matching the parent dir instead would fall through to
	// the overlay root and wrongly warn.
	dirMount, _ := longestMount(mounts, "/data")
	if !isEphemeralMount(dirMount) {
		t.Fatalf("expected /data to resolve to overlay root, got %#v", dirMount)
	}
}

func TestLongestMountNoRoot(t *testing.T) {
	// Without a root mount, an unrelated dir has no covering mount.
	mounts := []mountEntry{{mountPoint: "/data", fsType: "ext4"}}
	if _, ok := longestMount(mounts, "/elsewhere"); ok {
		t.Fatal("expected no covering mount")
	}
}

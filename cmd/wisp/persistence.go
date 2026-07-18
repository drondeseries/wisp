package main

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// mountEntry is the subset of a /proc/self/mountinfo line we care about.
type mountEntry struct {
	mountPoint string // field 5: where it is mounted
	fsType     string // post-separator field: the filesystem type
}

// warnDBPersistence logs a prominent warning when the pin database sits on the
// container's root/overlay filesystem instead of a mounted volume, where it
// would be lost on container recreation. It is best-effort: any parse or IO
// failure is swallowed silently so it can never affect startup.
func warnDBPersistence(dbPath string, log *slog.Logger) {
	abs, ok := absDBPath(dbPath)
	if !ok {
		return
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return // not Linux, or no procfs — can't tell, stay quiet
	}
	defer f.Close()
	mounts, err := parseMountinfo(f)
	if err != nil || len(mounts) == 0 {
		return
	}
	// Match against the DB *file* path, not its directory: when the file itself
	// is bind-mounted (e.g. `-v /host/wisp.db:/data/wisp.db`), the most-specific
	// covering mount is that file's own mount — matching the dir would miss it
	// and wrongly resolve to the overlay root.
	m, ok := longestMount(mounts, abs)
	if !ok {
		return // couldn't find a covering mount — don't guess
	}
	if isEphemeralMount(m) {
		log.Warn("pin database at "+abs+" is not on a mounted volume — pins will NOT survive container recreation; mount a volume at "+filepath.Dir(abs),
			"db_path", abs, "mount", m.mountPoint, "fstype", m.fsType)
	}
}

// absDBPath returns the absolute path of the pin database file.
func absDBPath(dbPath string) (string, bool) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return "", false
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", false
	}
	return abs, true
}

// parseMountinfo parses /proc/self/mountinfo. Each line is:
//
//	ID pID major:minor root mountpoint options [optional...] - fstype source superopts
//
// The variable-length optional fields end at a lone "-" separator, after which
// the filesystem type is the first field.
func parseMountinfo(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue // malformed — no separator or no fstype after it
		}
		out = append(out, mountEntry{
			mountPoint: unescapeMountField(fields[4]),
			fsType:     fields[sep+1],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// longestMount returns the mount whose mount point is the longest path prefix of
// dir — the mount that actually backs dir.
func longestMount(mounts []mountEntry, dir string) (mountEntry, bool) {
	best := mountEntry{}
	bestLen := -1
	for _, m := range mounts {
		if mountCovers(m.mountPoint, dir) && len(m.mountPoint) > bestLen {
			best, bestLen = m, len(m.mountPoint)
		}
	}
	return best, bestLen >= 0
}

// mountCovers reports whether mountPoint is dir or an ancestor of dir, matching
// on path boundaries so "/data" does not cover "/database".
func mountCovers(mountPoint, dir string) bool {
	mountPoint = strings.TrimRight(mountPoint, "/")
	dir = strings.TrimRight(dir, "/")
	if mountPoint == "" { // the root mount "/" trims to ""
		return true
	}
	return dir == mountPoint || strings.HasPrefix(dir, mountPoint+"/")
}

// isEphemeralMount reports whether a mount is the container's throwaway root:
// the root mount point, or an overlay/tmpfs filesystem. Data on these is lost
// when the container is recreated.
func isEphemeralMount(m mountEntry) bool {
	if strings.TrimRight(m.mountPoint, "/") == "" {
		return true // mounted at "/"
	}
	switch m.fsType {
	case "overlay", "overlayfs", "tmpfs":
		return true
	}
	return false
}

// unescapeMountField decodes the octal escapes (\040 space, \011 tab, \012
// newline, \134 backslash) the kernel uses for mountinfo path fields.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			switch s[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 3
				continue
			case "011":
				b.WriteByte('\t')
				i += 3
				continue
			case "012":
				b.WriteByte('\n')
				i += 3
				continue
			case "134":
				b.WriteByte('\\')
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

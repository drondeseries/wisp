package notify

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// plexSectionTTL is how long a fetched section list is trusted before a re-fetch.
const plexSectionTTL = 5 * time.Minute

// plexTarget triggers Plex partial-scans of the folders that changed. Plex scans
// directories, not files, so every event maps to a refresh of the affected
// folder(s) in the library section whose location contains that folder.
type plexTarget struct {
	baseURL    string
	token      string
	mountPath  string
	httpClient *http.Client
	log        *slog.Logger

	mu        sync.Mutex
	sections  []plexSection
	fetchedAt time.Time
}

// plexSection is one library section (its numeric key and on-disk locations).
type plexSection struct {
	Key       string
	Locations []string
}

// plexSectionsXML mirrors the Plex GET /library/sections response.
type plexSectionsXML struct {
	Directories []struct {
		Key       string `xml:"key,attr"`
		Locations []struct {
			Path string `xml:"path,attr"`
		} `xml:"Location"`
	} `xml:"Directory"`
}

func newPlexTarget(baseURL, token, mountPath string, log *slog.Logger) *plexTarget {
	return &plexTarget{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:      strings.TrimSpace(token),
		mountPath:  mountPath,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

func (t *plexTarget) name() string { return "plex" }

func (t *plexTarget) Import(ctx context.Context, _ /*mediaType*/, virtualPath string) {
	t.refreshDirs(ctx, "import", virtualPath)
}

func (t *plexTarget) Delete(ctx context.Context, _ /*mediaType*/, virtualPath string) {
	t.refreshDirs(ctx, "delete", virtualPath)
}

func (t *plexTarget) Rename(ctx context.Context, _ /*mediaType*/, previousPath, newPath string) {
	// Both the vacated and the new folder need a scan; refreshDirs dedupes when
	// they share a directory.
	t.refreshDirs(ctx, "rename", previousPath, newPath)
}

// refreshDirs maps the changed virtual paths to their absolute parent folders
// and issues a Plex partial refresh for each distinct folder.
func (t *plexTarget) refreshDirs(ctx context.Context, event string, virtualPaths ...string) {
	if t == nil || t.baseURL == "" {
		return
	}
	sections, err := t.getSections(ctx)
	if err != nil {
		t.log.Warn("plex section lookup failed", "event", event, "error", err)
		return
	}
	seen := map[string]bool{}
	for _, vp := range virtualPaths {
		dir := path.Dir(fullPath(t.mountPath, vp))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		key, ok := sectionForPath(sections, dir)
		if !ok {
			t.log.Warn("plex has no section covering path", "event", event, "path", dir)
			continue
		}
		t.refresh(ctx, event, key, dir)
	}
}

func (t *plexTarget) refresh(ctx context.Context, event, sectionKey, dir string) {
	q := url.Values{}
	q.Set("path", dir)
	target := fmt.Sprintf("%s/library/sections/%s/refresh?%s", t.baseURL, sectionKey, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.log.Warn("plex refresh request failed", "event", event, "error", err)
		return
	}
	req.Header.Set("User-Agent", "wisp")
	if t.token != "" {
		req.Header.Set("X-Plex-Token", t.token)
	}
	status, err := doAndDrain(t.httpClient, req)
	if err != nil {
		t.log.Warn("plex refresh delivery failed", "event", event, "error", err)
		return
	}
	if !okStatus(status) {
		t.log.Warn("plex refresh rejected", "event", event, "section", sectionKey, "status", status)
		return
	}
	t.log.Info("plex refreshed", "event", event, "section", sectionKey, "path", dir)
}

// getSections returns the cached section list, refetching when the cache is
// empty or older than plexSectionTTL. A failed refetch reuses a stale cache
// rather than dropping the notification.
func (t *plexTarget) getSections(ctx context.Context) ([]plexSection, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sections) > 0 && time.Since(t.fetchedAt) < plexSectionTTL {
		return t.sections, nil
	}
	sections, err := t.fetchSections(ctx)
	if err != nil {
		if len(t.sections) > 0 {
			t.log.Warn("plex section refetch failed; using cached sections", "error", err)
			return t.sections, nil
		}
		return nil, err
	}
	t.sections = sections
	t.fetchedAt = time.Now()
	return sections, nil
}

func (t *plexTarget) fetchSections(ctx context.Context) ([]plexSection, error) {
	target := t.baseURL + "/library/sections"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wisp")
	req.Header.Set("Accept", "application/xml")
	if t.token != "" {
		req.Header.Set("X-Plex-Token", t.token)
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !okStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("plex sections returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parsePlexSections(body)
}

// parsePlexSections extracts section keys and their locations from Plex's XML.
func parsePlexSections(xmlBody []byte) ([]plexSection, error) {
	var doc plexSectionsXML
	if err := xml.Unmarshal(xmlBody, &doc); err != nil {
		return nil, err
	}
	sections := make([]plexSection, 0, len(doc.Directories))
	for _, d := range doc.Directories {
		locs := make([]string, 0, len(d.Locations))
		for _, l := range d.Locations {
			if p := strings.TrimSpace(l.Path); p != "" {
				locs = append(locs, p)
			}
		}
		if d.Key != "" && len(locs) > 0 {
			sections = append(sections, plexSection{Key: d.Key, Locations: locs})
		}
	}
	return sections, nil
}

// sectionForPath returns the key of the section whose location is the longest
// path prefix of dir, so nested libraries resolve to the most specific one.
func sectionForPath(sections []plexSection, dir string) (string, bool) {
	bestKey := ""
	bestLen := -1
	for _, s := range sections {
		for _, loc := range s.Locations {
			if pathHasPrefix(dir, loc) && len(loc) > bestLen {
				bestKey, bestLen = s.Key, len(loc)
			}
		}
	}
	return bestKey, bestLen >= 0
}

// pathHasPrefix reports whether prefix is dir or an ancestor directory of dir,
// matching on path boundaries so /a/b never matches /a/bc.
func pathHasPrefix(dir, prefix string) bool {
	dir = strings.TrimRight(dir, "/")
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		// The location was root ("/"); it covers every absolute path. (Empty
		// locations are filtered out during parsing, so this only means root.)
		return true
	}
	if dir == prefix {
		return true
	}
	return strings.HasPrefix(dir, prefix+"/")
}

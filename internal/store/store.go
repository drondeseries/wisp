// Package store persists pinned stream selections that back virtual files.
//
// Pins are held in a single bbolt bucket keyed by virtual path. bbolt is a
// B+tree, so keys iterate in sorted order — directory listings are a cursor
// seek to the path prefix, which is exactly the access pattern wisp needs.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"go.etcd.io/bbolt"
)

var pinsBucket = []byte("pins")

// Pin is a virtual file backed by an AIOStreams-resolved stream.
type Pin struct {
	ID        int64
	MediaType string // "movie" | "series"
	IMDbID    string // "tt…" or a "tmdb:…" search fallback
	TMDbID    string // bare tmdb id, for Silo-side (tmdb-keyed) status matching
	TVDbID    string // bare tvdb id, when known (cheap to carry)
	Category  string // library root the title resolved to (library.Root*); the
	// first path segment of VirtualPath — inherited from the title's monitor,
	// never re-derived (a change would orphan the on-disk file).
	Season      int
	Episode     int
	Title       string
	Year        int
	Quality     string
	VirtualPath string // library-relative, forward-slash separated (the key)
	SourceURL   string // AIOStreams resolver permalink (re-unlocks debrid on each open)
	Size        int64
	ResolvedAt  time.Time
}

// Servable reports whether a pin currently has a resolvable stream: a non-empty
// source URL and a known size. This is the cheap health signal the status API
// checks before reporting a title completed — it does not touch the network.
func (p Pin) Servable() bool { return p.SourceURL != "" && p.Size > 0 }

// Store is a bbolt-backed pin repository.
type Store struct {
	db *bbolt.DB
}

// Open opens (and initializes) the pin database at path.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(pinsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(monitorsBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("init bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts or replaces a pin by its virtual path. A new pin is assigned a
// stable sequence ID; re-upserting the same path preserves it.
func (s *Store) Upsert(_ context.Context, p Pin) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pinsBucket)
		key := []byte(p.VirtualPath)
		if existing := b.Get(key); existing != nil {
			var old Pin
			if err := json.Unmarshal(existing, &old); err == nil {
				p.ID = old.ID
			}
		} else if p.ID == 0 {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			p.ID = int64(seq)
		}
		val, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return b.Put(key, val)
	})
}

// BackfillCategories is an additive, idempotent migration: it stamps a Category
// on any pin or monitor that predates the field, WITHOUT rewriting VirtualPaths.
// A pin's category comes from its existing path prefix (movies/→movies,
// shows/→shows); a monitor's comes from any pin already recorded for its media
// id, else a non-anime default for its media type. Records that already carry a
// category are left byte-identical. Returns the number of records updated.
func (s *Store) BackfillCategories(_ context.Context) (int, error) {
	type kv struct{ k, v []byte }
	updated := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		// Collect writes during iteration, then apply them after — mutating a bbolt
		// bucket inside its own ForEach can make the cursor skip or repeat keys.
		pins := tx.Bucket(pinsBucket)
		pinRoot := map[string]string{} // search id → category (for monitor inheritance)
		var pinWrites []kv
		if err := pins.ForEach(func(k, v []byte) error {
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.Category == "" {
				if root := library.RootOf(p.VirtualPath); root != "" {
					p.Category = root
					val, err := json.Marshal(p)
					if err != nil {
						return err
					}
					pinWrites = append(pinWrites, kv{append([]byte(nil), k...), val})
				}
			}
			if p.Category != "" && p.IMDbID != "" {
				pinRoot[p.IMDbID] = p.Category
			}
			return nil
		}); err != nil {
			return err
		}

		mons := tx.Bucket(monitorsBucket)
		var monWrites []kv
		if err := mons.ForEach(func(k, v []byte) error {
			var m Monitored
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if m.Category != "" {
				return nil
			}
			if root := pinRoot[monitoredSearchID(m)]; root != "" {
				m.Category = root
			} else {
				m.Category = library.Root(m.MediaType, false)
			}
			val, err := json.Marshal(m)
			if err != nil {
				return err
			}
			monWrites = append(monWrites, kv{append([]byte(nil), k...), val})
			return nil
		}); err != nil {
			return err
		}

		for _, w := range pinWrites {
			if err := pins.Put(w.k, w.v); err != nil {
				return err
			}
		}
		for _, w := range monWrites {
			if err := mons.Put(w.k, w.v); err != nil {
				return err
			}
		}
		updated = len(pinWrites) + len(monWrites)
		return nil
	})
	return updated, err
}

// ByPath returns the pin for a virtual path, or (nil, nil) if absent.
func (s *Store) ByPath(_ context.Context, virtualPath string) (*Pin, error) {
	var pin *Pin
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(pinsBucket).Get([]byte(virtualPath))
		if v == nil {
			return nil
		}
		var p Pin
		if err := json.Unmarshal(v, &p); err != nil {
			return err
		}
		pin = &p
		return nil
	})
	return pin, err
}

// UpdateResolution rewrites a pin's source URL and size after a re-resolve.
func (s *Store) UpdateResolution(_ context.Context, virtualPath, sourceURL string, size int64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pinsBucket)
		key := []byte(virtualPath)
		v := b.Get(key)
		if v == nil {
			return fmt.Errorf("pin not found: %s", virtualPath)
		}
		var p Pin
		if err := json.Unmarshal(v, &p); err != nil {
			return err
		}
		p.SourceURL, p.Size, p.ResolvedAt = sourceURL, size, time.Now()
		val, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return b.Put(key, val)
	})
}

// List returns every pin, ordered by virtual path.
func (s *Store) List(_ context.Context) ([]Pin, error) {
	var pins []Pin
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(pinsBucket).ForEach(func(_, v []byte) error {
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			pins = append(pins, p)
			return nil
		})
	})
	return pins, err
}

// Count returns the number of pins.
func (s *Store) Count(_ context.Context) (int, error) {
	n := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(pinsBucket).Stats().KeyN
		return nil
	})
	return n, err
}

// Delete removes the pin at a virtual path, reporting whether it existed.
func (s *Store) Delete(_ context.Context, virtualPath string) (bool, error) {
	existed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pinsBucket)
		if b.Get([]byte(virtualPath)) != nil {
			existed = true
		}
		return b.Delete([]byte(virtualPath))
	})
	return existed, err
}

// DeleteByMedia removes every pin matching an IMDb id (and, for series, a
// season/episode), returning the deleted virtual paths. Use season<=0 to match
// a movie. A non-empty quality further restricts deletion to that quality tier
// (compared in canonical form), so distinct 1080p/2160p pins can be removed
// individually.
func (s *Store) DeleteByMedia(_ context.Context, imdbID string, season, episode int, quality string) ([]string, error) {
	var deleted []string
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pinsBucket)
		var keys [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			// Normalize the stored label before comparing so a pin saved under a
			// non-canonical resolution (e.g. "4K" from an older version) still
			// matches a canonical delete request ("2160p").
			if p.IMDbID == imdbID &&
				(season <= 0 || (p.Season == season && p.Episode == episode)) &&
				(quality == "" || strings.EqualFold(library.NormalizeQuality(p.Quality), quality)) {
				keys = append(keys, append([]byte(nil), k...))
				deleted = append(deleted, p.VirtualPath)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	return deleted, err
}

// CategoryForMedia returns the library root an existing pin for a title already
// uses — matched by search id (imdb or "tmdb:<id>" slot) or by the persisted
// bare TMDbID — so a new monitor inherits the title's first-decided root instead
// of re-deciding it. Returns "" when no pin exists for the title.
func (s *Store) CategoryForMedia(_ context.Context, searchID, tmdbID string) string {
	category := ""
	_ = s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(pinsBucket).ForEach(func(_, v []byte) error {
			if category != "" {
				return nil // first match already found
			}
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if !pinMatchesID(p, searchID, tmdbID) {
				return nil
			}
			if c := p.Category; c != "" {
				category = c
			} else if root := library.RootOf(p.VirtualPath); root != "" {
				category = root
			}
			return nil
		})
	})
	return category
}

// PinsByTMDbID returns every pin whose persisted bare TMDbID matches, so a
// tmdb-only lookup finds imdb-keyed (legacy/direct) pins that the "tmdb:<id>"
// search-id convention would miss.
func (s *Store) PinsByTMDbID(_ context.Context, tmdbID string) ([]Pin, error) {
	if tmdbID == "" {
		return nil, nil
	}
	var pins []Pin
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(pinsBucket).ForEach(func(_, v []byte) error {
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.TMDbID == tmdbID {
				pins = append(pins, p)
			}
			return nil
		})
	})
	return pins, err
}

// pinMatchesID reports whether a pin belongs to a title identified by a search
// id (its IMDbID slot, which may hold "tmdb:<id>") or a bare tmdb id.
func pinMatchesID(p Pin, searchID, tmdbID string) bool {
	if searchID != "" && p.IMDbID == searchID {
		return true
	}
	return tmdbID != "" && p.TMDbID == tmdbID
}

// PinsByMedia returns every pin for an IMDb id (all seasons/episodes/qualities),
// so the monitor can dedupe without re-pinning what already exists.
func (s *Store) PinsByMedia(_ context.Context, imdbID string) ([]Pin, error) {
	var pins []Pin
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(pinsBucket).ForEach(func(_, v []byte) error {
			var p Pin
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.IMDbID == imdbID {
				pins = append(pins, p)
			}
			return nil
		})
	})
	return pins, err
}

// Children returns the immediate directory and file names under a virtual
// directory prefix (empty prefix = library root). dirs end without a slash.
//
// Because bbolt yields keys in sorted order, the scan seeks to prefix+"/" and
// stops at the first key that no longer shares it — no full-bucket scan.
func (s *Store) Children(_ context.Context, prefix string) (dirs, files []string, err error) {
	prefix = strings.Trim(prefix, "/")
	scanPrefix := ""
	if prefix != "" {
		scanPrefix = prefix + "/"
	}
	seenDir := map[string]bool{}
	seenFile := map[string]bool{}
	err = s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(pinsBucket).Cursor()
		var k []byte
		if scanPrefix == "" {
			k, _ = c.First()
		} else {
			k, _ = c.Seek([]byte(scanPrefix))
		}
		for ; k != nil; k, _ = c.Next() {
			vp := string(k)
			if scanPrefix != "" {
				if !strings.HasPrefix(vp, scanPrefix) {
					break // sorted order: no further children
				}
				vp = strings.TrimPrefix(vp, scanPrefix)
			}
			if vp == "" {
				continue
			}
			if idx := strings.IndexByte(vp, '/'); idx >= 0 {
				if name := vp[:idx]; !seenDir[name] {
					seenDir[name] = true
					dirs = append(dirs, name)
				}
			} else if !seenFile[vp] {
				seenFile[vp] = true
				files = append(files, vp)
			}
		}
		return nil
	})
	return dirs, files, err
}

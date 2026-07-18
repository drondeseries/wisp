// Package store persists pinned stream selections that back virtual files.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Pin is a virtual file backed by an AIOStreams-resolved stream.
type Pin struct {
	ID          int64
	MediaType   string // "movie" | "series"
	IMDbID      string
	Season      int
	Episode     int
	Title       string
	Year        int
	Quality     string
	VirtualPath string // library-relative, forward-slash separated
	SourceURL   string // AIOStreams resolver permalink (re-unlocks debrid on each open)
	Size        int64
	ResolvedAt  time.Time
}

// Store is a SQLite-backed pin repository.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the pin database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS pins (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	media_type   TEXT    NOT NULL,
	imdb_id      TEXT    NOT NULL,
	season       INTEGER NOT NULL DEFAULT 0,
	episode      INTEGER NOT NULL DEFAULT 0,
	title        TEXT    NOT NULL,
	year         INTEGER NOT NULL DEFAULT 0,
	quality      TEXT    NOT NULL DEFAULT '1080p',
	virtual_path TEXT    NOT NULL UNIQUE,
	source_url   TEXT    NOT NULL,
	size         INTEGER NOT NULL DEFAULT 0,
	resolved_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pins_lookup ON pins(imdb_id, season, episode);
`

// Upsert inserts or replaces a pin by its virtual path.
func (s *Store) Upsert(ctx context.Context, p Pin) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pins(media_type, imdb_id, season, episode, title, year, quality, virtual_path, source_url, size, resolved_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(virtual_path) DO UPDATE SET
			source_url=excluded.source_url, size=excluded.size, resolved_at=excluded.resolved_at`,
		p.MediaType, p.IMDbID, p.Season, p.Episode, p.Title, p.Year, p.Quality,
		p.VirtualPath, p.SourceURL, p.Size, p.ResolvedAt.Unix())
	return err
}

// ByPath returns the pin for a virtual path, or (nil, nil) if absent.
func (s *Store) ByPath(ctx context.Context, virtualPath string) (*Pin, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, media_type, imdb_id, season, episode, title, year, quality, virtual_path, source_url, size, resolved_at
		FROM pins WHERE virtual_path=?`, virtualPath)
	return scanPin(row)
}

// UpdateResolution rewrites the source URL and size after a re-resolve.
func (s *Store) UpdateResolution(ctx context.Context, id int64, sourceURL string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pins SET source_url=?, size=?, resolved_at=? WHERE id=?`,
		sourceURL, size, time.Now().Unix(), id)
	return err
}

// Children returns the immediate directory and file names under a virtual
// directory prefix (empty prefix = library root). dirs end without a slash.
func (s *Store) Children(ctx context.Context, prefix string) (dirs, files []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT virtual_path FROM pins`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	prefix = strings.Trim(prefix, "/")
	seenDir := map[string]bool{}
	seenFile := map[string]bool{}
	for rows.Next() {
		var vp string
		if err := rows.Scan(&vp); err != nil {
			return nil, nil, err
		}
		rest := vp
		if prefix != "" {
			if !strings.HasPrefix(vp, prefix+"/") {
				continue
			}
			rest = strings.TrimPrefix(vp, prefix+"/")
		}
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			name := rest[:idx]
			if !seenDir[name] {
				seenDir[name] = true
				dirs = append(dirs, name)
			}
		} else if !seenFile[rest] {
			seenFile[rest] = true
			files = append(files, rest)
		}
	}
	return dirs, files, rows.Err()
}

func scanPin(row *sql.Row) (*Pin, error) {
	var p Pin
	var resolvedAt int64
	err := row.Scan(&p.ID, &p.MediaType, &p.IMDbID, &p.Season, &p.Episode, &p.Title,
		&p.Year, &p.Quality, &p.VirtualPath, &p.SourceURL, &p.Size, &resolvedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.ResolvedAt = time.Unix(resolvedAt, 0)
	return &p, nil
}

package store

import (
	"context"
	"encoding/json"
	"time"

	"go.etcd.io/bbolt"
)

var monitorsBucket = []byte("monitors")

// Monitored is a title wisp is tracking until it can be pinned: a movie awaiting
// its home-media release/availability, or an ongoing series whose new episodes
// should be pinned as they air. It persists in the same bbolt DB as pins so the
// watchlist survives restarts.
type Monitored struct {
	Key       string    // stable id, e.g. "movie:tt123" or "series:tt456"
	MediaType string    // "movie" | "series"
	IMDbID    string    // "tt…" (may be empty for a tmdb-only movie)
	TMDbID    string    // stremio/tmdb id used against AIOStreams and TMDB
	TVDbID    string    // for folder tagging
	Title     string    // for folder/file naming
	Year      int       // for folder/file naming
	Qualities []string  // requested tiers; empty = default (best stream)
	Seasons   []int     // series: requested seasons; empty = all
	DueAt     time.Time // earliest time worth re-checking (release or next air)

	// Observability / control (kept-and-marked so the monitor list doubles as a
	// request history — idea from drondeseries's PR #5).
	Enabled     bool      // false = paused; kept but not refreshed
	Completed   bool      // movie: every requested quality is pinned
	LastChecked time.Time // when the scheduler last processed it
	LastError   string    // last non-fatal error, for surfacing in the CRUD API

	AddedAt   time.Time
	UpdatedAt time.Time
}

// PutMonitored inserts or replaces a monitored item by its key.
func (s *Store) PutMonitored(_ context.Context, m Monitored) error {
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now()
	}
	m.UpdatedAt = time.Now()
	return s.db.Update(func(tx *bbolt.Tx) error {
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return tx.Bucket(monitorsBucket).Put([]byte(m.Key), val)
	})
}

// GetMonitored returns the monitored item for a key, or (nil, nil) if absent.
func (s *Store) GetMonitored(_ context.Context, key string) (*Monitored, error) {
	var item *Monitored
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(monitorsBucket).Get([]byte(key))
		if v == nil {
			return nil
		}
		var m Monitored
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		item = &m
		return nil
	})
	return item, err
}

// ListMonitored returns every monitored item.
func (s *Store) ListMonitored(_ context.Context) ([]Monitored, error) {
	var items []Monitored
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(monitorsBucket).ForEach(func(_, v []byte) error {
			var m Monitored
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			items = append(items, m)
			return nil
		})
	})
	return items, err
}

// DeleteMonitored removes a monitored item (e.g. a movie that finished pinning).
func (s *Store) DeleteMonitored(_ context.Context, key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(monitorsBucket).Delete([]byte(key))
	})
}

// CountMonitored returns the number of monitored items (for observability).
func (s *Store) CountMonitored(_ context.Context) (int, error) {
	n := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(monitorsBucket).Stats().KeyN
		return nil
	})
	return n, err
}

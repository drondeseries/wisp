package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// airDateTolerance bounds how far a TVmaze airstamp may sit from Cinemeta's
// canonical date and still be trusted as the same episode. A genuine match
// differs only by timezone/date-vs-datetime (well under a day); a spurious
// (season, episode) key collision across diverged layouts (anime split cours)
// is weeks or months off. 48h covers the former without admitting the latter.
const airDateTolerance = 48 * time.Hour

// Episodes returns a series' canonical episode set (Cinemeta numbering) with air
// dates refined by TVmaze where the two providers corroborate. Episodes are
// sorted and de-duplicated by (season, number). Falls back to TVmaze numbering
// only if Cinemeta yields nothing.
func (s *Service) Episodes(ctx context.Context, imdbID string) ([]Episode, error) {
	if !strings.HasPrefix(strings.TrimSpace(imdbID), "tt") {
		return nil, fmt.Errorf("metadata: imdb id required to enumerate episodes")
	}
	canonical, cErr := s.cinemetaEpisodes(ctx, imdbID)
	tvmaze, tErr := s.tvmazeEpisodes(ctx, imdbID)

	if cErr == nil && len(canonical) > 0 {
		if tErr == nil {
			enrichAirDates(canonical, tvmaze)
		}
		return dedupeSort(canonical), nil
	}
	if tErr == nil && len(tvmaze) > 0 {
		return dedupeSort(tvmaze), nil // Cinemeta empty; TVmaze numbering is the fallback
	}
	if cErr != nil {
		return nil, fmt.Errorf("cinemeta: %w", cErr)
	}
	if tErr != nil {
		return nil, fmt.Errorf("tvmaze: %w", tErr)
	}
	return nil, fmt.Errorf("metadata: no episodes for %s", imdbID)
}

// ReleasedEpisodes returns the episodes that have aired as of now.
func (s *Service) ReleasedEpisodes(ctx context.Context, imdbID string, now time.Time) ([]Episode, error) {
	eps, err := s.Episodes(ctx, imdbID)
	if err != nil {
		return nil, err
	}
	out := eps[:0:0]
	for _, e := range eps {
		if !e.Aired.IsZero() && !e.Aired.After(now) {
			out = append(out, e)
		}
	}
	return out, nil
}

// NextAir returns the earliest episode air time strictly after `after`, so the
// scheduler can wake near a real airing instead of polling blindly. ok is false
// when no future air date is known.
func NextAir(eps []Episode, after time.Time) (t time.Time, ok bool) {
	for _, e := range eps {
		if e.Aired.After(after) && (!ok || e.Aired.Before(t)) {
			t, ok = e.Aired, true
		}
	}
	return t, ok
}

// MovieReleaseDate returns a movie's home-media release date, preferring TMDB's
// digital/physical dates and falling back to Cinemeta. It returns
// ErrNoHomeRelease when TMDB confirms the movie is theatrical-only (so the
// caller keeps monitoring rather than pinning a cam).
func (s *Service) MovieReleaseDate(ctx context.Context, imdbID, tmdbID string, now time.Time) (time.Time, error) {
	if s.HasTMDB() && strings.TrimSpace(tmdbID) != "" {
		t, err := s.MovieHomeRelease(ctx, tmdbID)
		if err == nil {
			return t, nil
		}
		if errors.Is(err, ErrNoHomeRelease) {
			return time.Time{}, err // theatrical-only — do not fall back to a theatrical date
		}
		// Transient TMDB failure: fall through to Cinemeta.
	}
	if strings.HasPrefix(strings.TrimSpace(imdbID), "tt") {
		return s.cinemetaMovieReleased(ctx, imdbID, now)
	}
	return time.Time{}, fmt.Errorf("metadata: no release-date source (need TMDB key or imdb id)")
}

// enrichAirDates overwrites Cinemeta's canonical air dates with TVmaze's more
// precise airstamps, but only when they corroborate within airDateTolerance —
// so a shared (season, number) key across diverged season layouts can't graft a
// wrong airstamp onto the canonical episode.
func enrichAirDates(canonical, tvmaze []Episode) {
	air := make(map[[2]int]time.Time, len(tvmaze))
	for _, e := range tvmaze {
		if e.Season > 0 && e.Number > 0 && !e.Aired.IsZero() {
			air[[2]int{e.Season, e.Number}] = e.Aired
		}
	}
	for i := range canonical {
		existing := canonical[i].Aired
		if existing.IsZero() { // nothing to corroborate against — keep it unknown
			continue
		}
		cand, ok := air[[2]int{canonical[i].Season, canonical[i].Number}]
		if !ok || absDuration(cand.Sub(existing)) > airDateTolerance {
			continue
		}
		canonical[i].Aired = cand
	}
}

// dedupeSort orders episodes by (season, number) and drops duplicate keys,
// keeping the first (which, after enrichment, carries the best air date).
func dedupeSort(eps []Episode) []Episode {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Season != eps[j].Season {
			return eps[i].Season < eps[j].Season
		}
		return eps[i].Number < eps[j].Number
	})
	out := eps[:0:0]
	var last [2]int
	for i, e := range eps {
		key := [2]int{e.Season, e.Number}
		if i > 0 && key == last {
			continue
		}
		out = append(out, e)
		last = key
	}
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

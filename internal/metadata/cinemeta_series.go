package metadata

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// cinemetaEpisodes returns a series' canonical episode set from Cinemeta's
// meta.videos — the numbering AIOStreams resolves against. Air dates here are
// coarse; enrichAirDates refines them from TVmaze.
func (s *Service) cinemetaEpisodes(ctx context.Context, imdbID string) ([]Episode, error) {
	var payload struct {
		Meta struct {
			Videos []struct {
				Season     int        `json:"season"`
				Episode    int        `json:"episode"`
				Number     int        `json:"number"`
				Released   *time.Time `json:"released"`
				FirstAired *time.Time `json:"firstAired"`
			} `json:"videos"`
		} `json:"meta"`
	}
	endpoint := s.cinemetaBase + "/meta/series/" + url.PathEscape(imdbID) + ".json"
	if err := s.getJSON(ctx, endpoint, &payload, nil); err != nil {
		return nil, err
	}
	eps := make([]Episode, 0, len(payload.Meta.Videos))
	for _, v := range payload.Meta.Videos {
		number := v.Episode
		if number <= 0 {
			number = v.Number
		}
		if v.Season <= 0 || number <= 0 { // skip specials / unnumbered
			continue
		}
		var aired time.Time
		switch {
		case v.Released != nil:
			aired = *v.Released
		case v.FirstAired != nil:
			aired = *v.FirstAired
		}
		eps = append(eps, Episode{Season: v.Season, Number: number, Aired: aired})
	}
	return eps, nil
}

// cinemetaMovieReleased returns a movie's release date from Cinemeta, falling
// back to a past release year when only that is known.
func (s *Service) cinemetaMovieReleased(ctx context.Context, imdbID string, now time.Time) (time.Time, error) {
	var payload struct {
		Meta struct {
			Released    *time.Time `json:"released"`
			ReleaseInfo string     `json:"releaseInfo"`
			Year        string     `json:"year"`
		} `json:"meta"`
	}
	endpoint := s.cinemetaBase + "/meta/movie/" + url.PathEscape(imdbID) + ".json"
	if err := s.getJSON(ctx, endpoint, &payload, nil); err != nil {
		return time.Time{}, err
	}
	if payload.Meta.Released != nil && !payload.Meta.Released.IsZero() {
		return *payload.Meta.Released, nil
	}
	for _, v := range []string{payload.Meta.ReleaseInfo, payload.Meta.Year} {
		v = strings.TrimSpace(v)
		if len(v) < 4 {
			continue
		}
		if year, err := strconv.Atoi(v[:4]); err == nil && year > 0 && year < now.Year() {
			return time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("cinemeta: no release date for %s", imdbID)
}

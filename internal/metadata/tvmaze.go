package metadata

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// tvmazeEpisodes returns a series' episodes with TVmaze's precise airstamps,
// looked up by IMDb id. The numbering is only used to key airstamps onto
// Cinemeta's canonical episodes (see enrichAirDates).
func (s *Service) tvmazeEpisodes(ctx context.Context, imdbID string) ([]Episode, error) {
	var show struct {
		ID int `json:"id"`
	}
	if err := s.getJSON(ctx, s.tvmazeBase+"/lookup/shows?imdb="+url.QueryEscape(imdbID), &show, nil); err != nil {
		return nil, err
	}
	if show.ID <= 0 {
		return nil, nil
	}
	var raw []struct {
		Season   int        `json:"season"`
		Number   int        `json:"number"`
		Airstamp *time.Time `json:"airstamp"`
		Airdate  string     `json:"airdate"`
	}
	if err := s.getJSON(ctx, s.tvmazeBase+"/shows/"+strconv.Itoa(show.ID)+"/episodes", &raw, nil); err != nil {
		return nil, err
	}
	eps := make([]Episode, 0, len(raw))
	for _, e := range raw {
		var aired time.Time
		switch {
		case e.Airstamp != nil:
			aired = *e.Airstamp
		case e.Airdate != "":
			if t, err := time.Parse("2006-01-02", e.Airdate); err == nil {
				aired = t
			}
		}
		eps = append(eps, Episode{Season: e.Season, Number: e.Number, Aired: aired})
	}
	return eps, nil
}

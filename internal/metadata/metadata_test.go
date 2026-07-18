package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// testService points every provider base URL at one mux.
func testService(t *testing.T, mux *http.ServeMux) *Service {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := New("v3key", []string{"US"})
	s.cinemetaBase, s.tmdbBase, s.tvmazeBase = srv.URL, srv.URL, srv.URL
	return s
}

func TestSelectHomeRelease(t *testing.T) {
	payload := tmdbReleaseDates{}
	payload.Results = []struct {
		Country string `json:"iso_3166_1"`
		Dates   []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		} `json:"release_dates"`
	}{
		{Country: "US", Dates: []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		}{{Date: date("2026-03-01T00:00:00Z"), Type: 3}, {Date: date("2026-04-10T00:00:00Z"), Type: 4}}},
		{Country: "GB", Dates: []struct {
			Date time.Time `json:"release_date"`
			Type int       `json:"type"`
		}{{Date: date("2026-04-05T00:00:00Z"), Type: 4}}},
	}
	// US only → digital 04-10 (ignores theatrical type 3).
	if got, ok := selectHomeRelease(payload, []string{"US"}); !ok || !got.Equal(date("2026-04-10T00:00:00Z")) {
		t.Fatalf("US = %v %v", got, ok)
	}
	// US+GB → earliest digital across markets = GB 04-05.
	if got, ok := selectHomeRelease(payload, []string{"US", "GB"}); !ok || !got.Equal(date("2026-04-05T00:00:00Z")) {
		t.Fatalf("US+GB = %v %v", got, ok)
	}
	// A theatrical-only movie → no home release.
	theatrical := tmdbReleaseDates{}
	theatrical.Results = payload.Results[:1]
	theatrical.Results[0].Dates = theatrical.Results[0].Dates[:1] // just the type-3
	if _, ok := selectHomeRelease(theatrical, []string{"US"}); ok {
		t.Fatal("theatrical-only should have no home release")
	}
}

func TestEnrichAirDates(t *testing.T) {
	canonical := []Episode{
		{Season: 1, Number: 1, Aired: date("2026-01-01T00:00:00Z")},
		{Season: 1, Number: 2, Aired: date("2026-01-08T00:00:00Z")},
		{Season: 1, Number: 3}, // no canonical date
	}
	tvmaze := []Episode{
		{Season: 1, Number: 1, Aired: date("2026-01-01T02:30:00Z")}, // within 48h → enriches
		{Season: 1, Number: 2, Aired: date("2026-06-01T00:00:00Z")}, // months off → rejected
		{Season: 1, Number: 3, Aired: date("2026-01-15T00:00:00Z")}, // canonical zero → not trusted
	}
	enrichAirDates(canonical, tvmaze)
	if !canonical[0].Aired.Equal(date("2026-01-01T02:30:00Z")) {
		t.Fatalf("E1 not enriched: %v", canonical[0].Aired)
	}
	if !canonical[1].Aired.Equal(date("2026-01-08T00:00:00Z")) {
		t.Fatalf("E2 wrongly overwritten: %v", canonical[1].Aired)
	}
	if !canonical[2].Aired.IsZero() {
		t.Fatalf("E3 should stay unknown (no canonical date to corroborate): %v", canonical[2].Aired)
	}
}

func TestNextAir(t *testing.T) {
	eps := []Episode{
		{Season: 1, Number: 1, Aired: date("2026-01-01T00:00:00Z")},
		{Season: 1, Number: 2, Aired: date("2026-01-15T00:00:00Z")},
		{Season: 1, Number: 3, Aired: date("2026-01-08T00:00:00Z")},
	}
	now := date("2026-01-05T00:00:00Z")
	got, ok := NextAir(eps, now)
	if !ok || !got.Equal(date("2026-01-08T00:00:00Z")) {
		t.Fatalf("NextAir = %v %v, want 01-08", got, ok)
	}
	if _, ok := NextAir(eps, date("2026-02-01T00:00:00Z")); ok {
		t.Fatal("no future air expected after all episodes")
	}
}

func TestEpisodesMergeAndRelease(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta/series/tt1.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"videos":[
			{"season":1,"episode":1,"released":"2026-01-01T00:00:00Z"},
			{"season":1,"episode":2,"released":"2026-01-08T00:00:00Z"},
			{"season":1,"episode":3,"released":"2027-01-01T00:00:00Z"},
			{"season":0,"episode":1,"released":"2025-12-01T00:00:00Z"}
		]}}`))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":42}`))
	})
	mux.HandleFunc("/shows/42/episodes", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"season":1,"number":1,"airstamp":"2026-01-01T01:00:00Z"}]`))
	})
	s := testService(t, mux)

	all, err := s.Episodes(context.Background(), "tt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 { // season-0 special dropped
		t.Fatalf("episodes = %d, want 3", len(all))
	}
	if !all[0].Aired.Equal(date("2026-01-01T01:00:00Z")) {
		t.Fatalf("E1 airstamp not enriched from TVmaze: %v", all[0].Aired)
	}
	released, err := s.ReleasedEpisodes(context.Background(), "tt1", date("2026-02-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 { // E3 airs 2027 → excluded
		t.Fatalf("released = %d, want 2", len(released))
	}
}

func TestMovieReleaseDate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movie/500/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":4,"release_date":"2026-05-01T00:00:00Z"}]}]}`))
	})
	// Theatrical-only movie.
	mux.HandleFunc("/movie/600/release_dates", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"results":[{"iso_3166_1":"US","release_dates":[{"type":3,"release_date":"2026-05-01T00:00:00Z"}]}]}`))
	})
	mux.HandleFunc("/meta/movie/tt9.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"meta":{"released":"2020-02-02T00:00:00Z"}}`))
	})
	s := testService(t, mux)
	now := date("2026-06-01T00:00:00Z")

	if got, err := s.MovieReleaseDate(context.Background(), "tt5", "500", now); err != nil || !got.Equal(date("2026-05-01T00:00:00Z")) {
		t.Fatalf("digital = %v, %v", got, err)
	}
	if _, err := s.MovieReleaseDate(context.Background(), "tt6", "600", now); !errors.Is(err, ErrNoHomeRelease) {
		t.Fatalf("theatrical-only err = %v, want ErrNoHomeRelease", err)
	}
	// No tmdb id → Cinemeta fallback.
	if got, err := s.MovieReleaseDate(context.Background(), "tt9", "", now); err != nil || !got.Equal(date("2020-02-02T00:00:00Z")) {
		t.Fatalf("cinemeta fallback = %v, %v", got, err)
	}
}

func TestIMDbForTMDB(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tv/77/external_ids", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "v3key" {
			t.Fatalf("missing v3 api_key: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"imdb_id":"tt77"}`))
	})
	s := testService(t, mux)
	got, err := s.IMDbForTMDB(context.Background(), "series", "77")
	if err != nil || got != "tt77" {
		t.Fatalf("IMDbForTMDB = %q, %v", got, err)
	}
}

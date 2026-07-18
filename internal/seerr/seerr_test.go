package seerr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseWebhookApprovedSeries(t *testing.T) {
	body := []byte(`{
		"notification_type":"MEDIA_AUTO_APPROVED",
		"subject":"The Villager of Level 999 (2026)",
		"media":{"media_type":"tv","tmdbId":"38262097","tvdbId":"467127","imdbId":"tt38262097"},
		"request":{"request_id":"12","is4k":false},
		"extra":[{"name":"Requested Seasons","value":"1, 2"}]
	}`)
	in, actionable, err := ParseWebhook(body)
	if err != nil || !actionable {
		t.Fatalf("actionable=%v err=%v", actionable, err)
	}
	if in.MediaType != "series" || in.IMDbID != "tt38262097" || in.TVDbID != "467127" {
		t.Fatalf("intake = %#v", in)
	}
	if in.Title != "The Villager of Level 999" || in.Year != 2026 {
		t.Fatalf("title/year = %q %d", in.Title, in.Year)
	}
	if !reflect.DeepEqual(in.Seasons, []int{1, 2}) {
		t.Fatalf("seasons = %v", in.Seasons)
	}
	if in.RequestID != 12 {
		t.Fatalf("request id = %d", in.RequestID)
	}
	if got := in.Qualities(); !reflect.DeepEqual(got, []string{"1080p"}) {
		t.Fatalf("qualities = %v, want [1080p]", got)
	}
}

func TestParseWebhook4KMovieQualities(t *testing.T) {
	body := []byte(`{"notification_type":"MEDIA_APPROVED","subject":"Inception (2010)",
		"media":{"media_type":"movie","tmdbId":"27205"},"request":{"is4k":true}}`)
	in, actionable, err := ParseWebhook(body)
	if err != nil || !actionable {
		t.Fatalf("actionable=%v err=%v", actionable, err)
	}
	if !in.Is4K || in.MediaType != "movie" {
		t.Fatalf("intake = %#v", in)
	}
	if got := in.Qualities(); !reflect.DeepEqual(got, []string{"2160p"}) {
		t.Fatalf("qualities = %v, want [2160p]", got)
	}
}

func TestParseWebhookIgnoresNonApproval(t *testing.T) {
	for _, nt := range []string{"TEST_NOTIFICATION", "MEDIA_PENDING", "MEDIA_AVAILABLE", "MEDIA_DECLINED"} {
		body := []byte(`{"notification_type":"` + nt + `","media":{"tmdbId":"1"}}`)
		_, actionable, err := ParseWebhook(body)
		if err != nil || actionable {
			t.Fatalf("%s: actionable=%v err=%v (want ignored)", nt, actionable, err)
		}
	}
}

func TestClientEnrichFillsGaps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/request/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Fatalf("missing api key")
		}
		w.Write([]byte(`{"is4k":true,"media":{"tmdbId":"27205","imdbId":"tt1375666"},"seasons":[{"seasonNumber":3}]}`))
	})
	mux.HandleFunc("/api/v1/movie/27205", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"title":"Inception","releaseDate":"2010-07-16"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "secret")
	in := &Intake{MediaType: "movie", RequestID: 7} // webhook lacked ids, 4k, title
	c.Enrich(context.Background(), in)

	if !in.Is4K || in.TMDbID != "27205" || in.IMDbID != "tt1375666" {
		t.Fatalf("enriched intake = %#v", in)
	}
	if in.Title != "Inception" || in.Year != 2010 {
		t.Fatalf("title/year = %q %d", in.Title, in.Year)
	}
	if len(in.Seasons) != 1 || in.Seasons[0] != 3 {
		t.Fatalf("seasons = %v", in.Seasons)
	}
}

func TestClientEnrichNoopWhenUnconfigured(t *testing.T) {
	in := &Intake{MediaType: "movie", RequestID: 7, TMDbID: "1"}
	New("", "").Enrich(context.Background(), in) // must not panic or change anything
	if in.TMDbID != "1" {
		t.Fatalf("intake mutated: %#v", in)
	}
}

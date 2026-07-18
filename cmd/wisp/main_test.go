package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

func TestProbeSizeUsesRangeTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=0-0" {
			t.Fatalf("probe request = %s range %q", r.Method, r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-0/987654321")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "x")
	}))
	defer server.Close()

	size, err := probeSize(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if size != 987654321 {
		t.Fatalf("size = %d", size)
	}
}

func TestProbeSizeAcceptsRangeIgnoredWithLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "123456")
	}))
	defer server.Close()

	size, err := probeSize(context.Background(), server.URL)
	if err != nil || size != 123456 {
		t.Fatalf("size = %d, err = %v", size, err)
	}
}

func TestProbeSizeRejectsResolverErrorPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, "not media")
	}))
	defer server.Close()

	if _, err := probeSize(context.Background(), server.URL); err == nil {
		t.Fatal("expected probe error")
	}
}

func TestSelectPlayableStreamFallsThrough(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/7654321")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer good.Close()

	streams := []aiostreams.Stream{
		{URL: bad.URL, Filename: "bad.mkv"},
		{URL: good.URL, Filename: "good.mp4", Resolution: "1080p"},
	}
	stream, size, err := selectPlayableStream(context.Background(), streams)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Filename != "good.mp4" || size != 7654321 {
		t.Fatalf("stream = %#v, size = %d", stream, size)
	}
}

package main

import (
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testDomain = "lab.patoarchitekci.io"

func TestParseTargetUp(t *testing.T) {
	// Recorded from loadgen 1.0.5 in the cluster.
	f, err := os.Open("testdata/loadgen-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := parseTargetUp(f, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"p-test": true, "p-test2": true, "p01-demo": true, "p02-demo": true}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	got, _ = parseTargetUp(strings.NewReader(`loadgen_target_up{target="https://p01-demo.lab.patoarchitekci.io"} 0
loadgen_target_up{target="http://orders:8080"} 1
loadgen_requests_total{target="https://p02-demo.lab.patoarchitekci.io",code="502"} 3
`), testDomain)
	if want := map[string]bool{"p01-demo": false}; !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNamespaceOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://p01-demo.lab.patoarchitekci.io":      "p01-demo",
		"https://p01-demo.lab.patoarchitekci.io:443/": "p01-demo",
		"https://a.b.lab.patoarchitekci.io":           "",
		"https://lab.patoarchitekci.io":               "",
		"https://p01-demo.example.com":                "",
		"http://orders:8080":                          "",
	} {
		if got := namespaceOf(in, testDomain); got != want {
			t.Errorf("namespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealthState(t *testing.T) {
	h := newHealth("", testDomain, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t0 := time.Date(2026, 10, 3, 14, 5, 0, 0, time.UTC)
	now := t0
	h.now = func() time.Time { return now }

	if got := h.get("p01-demo"); got.State != "unknown" || got.Since != "" {
		t.Errorf("no data: %+v", got)
	}
	h.apply(map[string]bool{"p01-demo": true}, t0)
	now = t0.Add(15 * time.Second)
	h.apply(map[string]bool{"p01-demo": true}, now) // no change: since stays
	if got := h.get("p01-demo"); got != (appHealth{"up", ts(t0)}) {
		t.Errorf("up: %+v", got)
	}
	now = t0.Add(30 * time.Second)
	h.apply(map[string]bool{"p01-demo": false}, now)
	if got := h.get("p01-demo"); got != (appHealth{"down", ts(now)}) {
		t.Errorf("down: %+v", got)
	}
	if got := h.get("p02-demo"); got.State != "unknown" {
		t.Errorf("not reported: %+v", got)
	}

	// Scrapes fail from here on: the last state holds for 60 s, then unknown.
	now = t0.Add(30*time.Second + healthStaleAfter)
	if got := h.get("p01-demo"); got.State != "down" {
		t.Errorf("before stale: %+v", got)
	}
	now = now.Add(time.Second)
	if got := h.get("p01-demo"); got.State != "unknown" {
		t.Errorf("stale: %+v", got)
	}

	// A target loadgen stops reporting is forgotten.
	h.apply(map[string]bool{}, now)
	if got := h.get("p01-demo"); got.State != "unknown" {
		t.Errorf("dropped: %+v", got)
	}
}

func TestHealthScrape(t *testing.T) {
	body, err := os.ReadFile("testdata/loadgen-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	h := newHealth(srv.URL, testDomain, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h.scrape(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.get("p01-demo"); got.State != "up" {
		t.Errorf("p01-demo: %+v", got)
	}
}

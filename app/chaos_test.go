package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	for _, s := range []string{"crash", "oom", "unready", "slow"} {
		if m, err := parseMode(s); err != nil || string(m) != s {
			t.Errorf("parseMode(%q) = %q, %v", s, m, err)
		}
	}
	for _, s := range []string{"", "CRASH", "boom"} {
		if _, err := parseMode(s); err == nil {
			t.Errorf("parseMode(%q) want error", s)
		}
	}
}

func newTestServer(ch *chaos) *httptest.Server {
	mux := http.NewServeMux()
	registerCommon(mux, "inventory", ch)
	mux.HandleFunc("POST /reserve", versionHandler("inventory"))
	return httptest.NewServer(ch.slowdown(mux))
}

func post(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestUnready(t *testing.T) {
	srv := newTestServer(newChaos())
	defer srv.Close()

	if got := get(t, srv.URL+"/readyz"); got != http.StatusOK {
		t.Fatalf("readyz before = %d, want 200", got)
	}
	if got := post(t, srv.URL+"/chaos/unready"); got != http.StatusAccepted {
		t.Fatalf("chaos/unready = %d, want 202", got)
	}
	if got := get(t, srv.URL+"/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz after = %d, want 503", got)
	}
	if got := get(t, srv.URL+"/healthz"); got != http.StatusOK {
		t.Fatalf("healthz after = %d, want 200", got)
	}
}

func TestSlow(t *testing.T) {
	ch := newChaos()
	ch.delay = 200 * time.Millisecond
	srv := newTestServer(ch)
	defer srv.Close()

	if got := post(t, srv.URL+"/chaos/slow"); got != http.StatusAccepted {
		t.Fatalf("chaos/slow = %d, want 202", got)
	}
	start := time.Now()
	post(t, srv.URL+"/reserve")
	if d := time.Since(start); d < ch.delay {
		t.Errorf("reserve took %v, want >= %v", d, ch.delay)
	}
	start = time.Now()
	get(t, srv.URL+"/healthz")
	if d := time.Since(start); d >= ch.delay {
		t.Errorf("healthz took %v, want no delay", d)
	}
}

func TestChaosUnknownMode(t *testing.T) {
	srv := newTestServer(newChaos())
	defer srv.Close()
	if got := post(t, srv.URL+"/chaos/boom"); got != http.StatusBadRequest {
		t.Fatalf("chaos/boom = %d, want 400", got)
	}
}

func TestCrashExitsWithOne(t *testing.T) {
	ch := newChaos()
	code := make(chan int, 1)
	ch.exit = func(c int) { code <- c }
	srv := newTestServer(ch)
	defer srv.Close()

	if got := post(t, srv.URL+"/chaos/crash"); got != http.StatusAccepted {
		t.Fatalf("chaos/crash = %d, want 202", got)
	}
	select {
	case c := <-code:
		if c != 1 {
			t.Fatalf("exit code = %d, want 1", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no exit")
	}
}

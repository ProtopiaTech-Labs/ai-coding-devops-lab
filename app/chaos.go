package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type mode string

const (
	modeCrash   mode = "crash"
	modeOOM     mode = "oom"
	modeUnready mode = "unready"
	modeSlow    mode = "slow"
)

func parseMode(s string) (mode, error) {
	switch m := mode(s); m {
	case modeCrash, modeOOM, modeUnready, modeSlow:
		return m, nil
	}
	return "", fmt.Errorf("unknown chaos mode %q", s)
}

// chaos holds the failure switches of one service.
type chaos struct {
	unready atomic.Bool
	slow    atomic.Bool
	delay   time.Duration // slow mode delay
	exit    func(int)     // os.Exit, replaceable in tests
}

func newChaos() *chaos {
	return &chaos{delay: 5 * time.Second, exit: os.Exit}
}

func (c *chaos) apply(m mode) {
	switch m {
	case modeCrash:
		c.exit(1)
	case modeOOM:
		go allocateForever()
	case modeUnready:
		c.unready.Store(true)
	case modeSlow:
		c.slow.Store(true)
	}
}

// handle serves POST /chaos/{mode}. unready and slow take effect before the
// 202, so the next request already sees them. crash and oom kill the process,
// so they answer first and act after.
func (c *chaos) handle(w http.ResponseWriter, r *http.Request) {
	m, err := parseMode(r.PathValue("mode"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if m == modeUnready || m == modeSlow {
		c.apply(m)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"chaos": string(m)})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if m == modeCrash {
		go func() { time.Sleep(100 * time.Millisecond); c.exit(1) }()
		return
	}
	if m == modeOOM {
		c.apply(m)
	}
}

// slowdown delays every response by c.delay while slow mode is on.
// Probes and chaos endpoints are not delayed, so slow mode shows as slow
// responses and not as failed liveness probes.
func (c *chaos) slowdown(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.slow.Load() && !exempt(r.URL.Path) {
			select {
			case <-time.After(c.delay):
			case <-r.Context().Done():
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func exempt(path string) bool {
	return path == "/healthz" || path == "/readyz" || strings.HasPrefix(path, "/chaos/")
}

// allocateForever touches new memory until the kernel kills the process.
func allocateForever() {
	var hold [][]byte
	for {
		b := make([]byte, 8<<20)
		for i := range b {
			b[i] = 1
		}
		hold = append(hold, b)
	}
}

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	healthEvery      = 15 * time.Second
	healthStaleAfter = 60 * time.Second
)

// appHealth is what the UI and GET /api/namespaces show per namespace.
type appHealth struct {
	State string `json:"state"`           // up, down or unknown
	Since string `json:"since,omitempty"` // last change, empty when unknown
}

// health keeps the loadgen view of every namespace in memory: up when the
// last POST /orders returned 201. No database; after a restart the first
// scrape fills it again.
type health struct {
	url    string
	domain string
	client *http.Client // internal Service, not the participant outbound guard
	log    *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	m      map[string]nsHealth
	lastOK time.Time
}

type nsHealth struct {
	up    bool
	since time.Time
}

func newHealth(metricsURL, domain string, log *slog.Logger) *health {
	return &health{
		url:    metricsURL,
		domain: domain,
		client: &http.Client{Timeout: 5 * time.Second},
		log:    log,
		now:    time.Now,
		m:      map[string]nsHealth{},
	}
}

// get returns the state of ns; unknown without data or when the last good
// scrape is older than healthStaleAfter.
func (h *health) get(ns string) appHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.m[ns]
	if !ok || h.now().Sub(h.lastOK) > healthStaleAfter {
		return appHealth{State: "unknown"}
	}
	st := "down"
	if e.up {
		st = "up"
	}
	return appHealth{State: st, Since: ts(e.since)}
}

// apply stores one scrape: since moves only when the state changes, and
// namespaces loadgen no longer reports are forgotten.
func (h *health) apply(up map[string]bool, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ns, u := range up {
		if e, ok := h.m[ns]; !ok || e.up != u {
			h.m[ns] = nsHealth{up: u, since: now}
		}
	}
	for ns := range h.m {
		if _, ok := up[ns]; !ok {
			delete(h.m, ns)
		}
	}
	h.lastOK = now
}

func (h *health) scrape(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	up, err := parseTargetUp(io.LimitReader(resp.Body, 4<<20), h.domain)
	if err != nil {
		return err
	}
	h.apply(up, h.now())
	return nil
}

// run scrapes now and then every healthEvery until ctx ends. A failed scrape
// keeps the last state, which turns unknown after healthStaleAfter.
func (h *health) run(ctx context.Context) {
	tick := time.NewTicker(healthEvery)
	defer tick.Stop()
	for {
		if err := h.scrape(ctx); err != nil {
			h.log.Warn("health: scrape failed, keeping last state", "url", h.url, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// parseTargetUp reads loadgen_target_up{target="https://<ns>.<domain>"} lines
// from a Prometheus text body. Other metrics and other hosts are skipped.
func parseTargetUp(r io.Reader, domain string) (map[string]bool, error) {
	const prefix = `loadgen_target_up{target="`
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), prefix)
		if !ok {
			continue
		}
		// Target URLs carry no quotes or backslashes, so the first `"}` ends the label.
		target, value, ok := strings.Cut(line, `"} `)
		if !ok {
			continue
		}
		if ns := namespaceOf(target, domain); ns != "" {
			out[ns] = strings.TrimSpace(value) == "1"
		}
	}
	return out, sc.Err()
}

// namespaceOf maps https://<ns>.<domain> to ns, or "".
func namespaceOf(target, domain string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	ns, ok := strings.CutSuffix(u.Hostname(), "."+domain)
	if !ok || ns == "" || strings.Contains(ns, ".") {
		return ""
	}
	return ns
}

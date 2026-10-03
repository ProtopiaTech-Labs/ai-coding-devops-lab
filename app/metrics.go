package main

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics holds loadgen results per target and writes them in the Prometheus
// text format 0.0.4. Counters survive a target leaving discovery; the up gauge
// does not, so a removed namespace stops being reported as up or down.
type metrics struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // {target, code}
	durSum   map[string]float64
	durCount map[string]uint64
	up       map[string]bool
	active   map[string]bool
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[[2]string]uint64{},
		durSum:   map[string]float64{},
		durCount: map[string]uint64{},
		up:       map[string]bool{},
		active:   map[string]bool{},
	}
}

// setTargets records the current discovery result and drops the up gauge of
// targets that are gone.
func (m *metrics) setTargets(targets []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = map[string]bool{}
	for _, t := range targets {
		m.active[t] = true
	}
	for t := range m.up {
		if !m.active[t] {
			delete(m.up, t)
		}
	}
}

// observe records one request. status 0 means a transport error.
func (m *metrics) observe(target string, status int, d time.Duration) {
	code := "error"
	if status != 0 {
		code = strconv.Itoa(status)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{target, code}]++
	m.durSum[target] += d.Seconds()
	m.durCount[target]++
	if m.active[target] {
		m.up[target] = status == http.StatusCreated
	}
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP loadgen_requests_total POST /orders requests sent, by target and HTTP status (error = no response).")
	fmt.Fprintln(w, "# TYPE loadgen_requests_total counter")
	keys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	for _, k := range keys {
		fmt.Fprintf(w, "loadgen_requests_total{target=\"%s\",code=\"%s\"} %d\n", escapeLabel(k[0]), escapeLabel(k[1]), m.requests[k])
	}

	fmt.Fprintln(w, "# HELP loadgen_request_duration_seconds Duration of POST /orders requests.")
	fmt.Fprintln(w, "# TYPE loadgen_request_duration_seconds summary")
	for _, t := range sortedKeys(m.durCount) {
		fmt.Fprintf(w, "loadgen_request_duration_seconds_sum{target=\"%s\"} %s\n", escapeLabel(t), strconv.FormatFloat(m.durSum[t], 'g', -1, 64))
		fmt.Fprintf(w, "loadgen_request_duration_seconds_count{target=\"%s\"} %d\n", escapeLabel(t), m.durCount[t])
	}

	fmt.Fprintln(w, "# HELP loadgen_target_up 1 when the last POST /orders to the target returned 201, else 0.")
	fmt.Fprintln(w, "# TYPE loadgen_target_up gauge")
	for _, t := range sortedKeys(m.up) {
		v := 0
		if m.up[t] {
			v = 1
		}
		fmt.Fprintf(w, "loadgen_target_up{target=\"%s\"} %d\n", escapeLabel(t), v)
	}
}

func (m *metrics) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.write(w)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

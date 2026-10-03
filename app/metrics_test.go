package main

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsText(t *testing.T) {
	a, b := "https://a.lab.example", "https://b.lab.example"
	m := newMetrics()
	m.setTargets([]string{a, b})
	m.observe(a, 201, 250*time.Millisecond)
	m.observe(a, 201, 500*time.Millisecond)
	m.observe(b, 201, 100*time.Millisecond)
	m.observe(b, 502, 100*time.Millisecond)
	m.observe(b, 0, time.Second)

	var sb strings.Builder
	m.write(&sb)
	want := `# HELP loadgen_requests_total POST /orders requests sent, by target and HTTP status (error = no response).
# TYPE loadgen_requests_total counter
loadgen_requests_total{target="https://a.lab.example",code="201"} 2
loadgen_requests_total{target="https://b.lab.example",code="201"} 1
loadgen_requests_total{target="https://b.lab.example",code="502"} 1
loadgen_requests_total{target="https://b.lab.example",code="error"} 1
# HELP loadgen_request_duration_seconds Duration of POST /orders requests.
# TYPE loadgen_request_duration_seconds summary
loadgen_request_duration_seconds_sum{target="https://a.lab.example"} 0.75
loadgen_request_duration_seconds_count{target="https://a.lab.example"} 2
loadgen_request_duration_seconds_sum{target="https://b.lab.example"} 1.2
loadgen_request_duration_seconds_count{target="https://b.lab.example"} 3
# HELP loadgen_target_up 1 when the last POST /orders to the target returned 201, else 0.
# TYPE loadgen_target_up gauge
loadgen_target_up{target="https://a.lab.example"} 1
loadgen_target_up{target="https://b.lab.example"} 0
`
	if got := sb.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// b leaves discovery: its gauge goes, its counters stay.
	m.setTargets([]string{a})
	sb.Reset()
	m.write(&sb)
	got := sb.String()
	if strings.Contains(got, `loadgen_target_up{target="https://b.lab.example"}`) {
		t.Error("removed target still has loadgen_target_up")
	}
	if !strings.Contains(got, `loadgen_requests_total{target="https://b.lab.example",code="502"} 1`) {
		t.Error("removed target lost its counters")
	}
}

func TestEscapeLabel(t *testing.T) {
	if got, want := escapeLabel("a\\b\"c\nd"), `a\\b\"c\nd`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

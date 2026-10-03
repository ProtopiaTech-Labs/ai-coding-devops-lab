package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func apiCall(t *testing.T, s *server, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("X-Api-Key", key)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return s.serve(r)
}

func TestAPIAuthMatrix(t *testing.T) {
	s := newTestServer(t)
	participantEP := [][2]string{
		{"GET", "/api/namespaces"}, {"GET", "/api/webhook"}, {"PUT", "/api/webhook-url"},
		{"POST", "/api/webhook/test"}, {"POST", "/api/webhook/replay"}, {"GET", "/api/deliveries"},
	}
	adminEP := [][2]string{{"GET", "/api/participants"}, {"POST", "/api/journal"}, {"PATCH", "/api/journal/1"}}
	both := [][2]string{{"GET", "/api/me"}, {"GET", "/api/journal"}}

	check := func(ep [2]string, key string, want int) {
		t.Helper()
		if w := apiCall(t, s, ep[0], ep[1], key, ""); w.Code != want {
			t.Errorf("%s %s key=%q: %d, want %d (%s)", ep[0], ep[1], key, w.Code, want, w.Body)
		}
	}
	for _, ep := range append(append(participantEP, adminEP...), both...) {
		check(ep, "", http.StatusUnauthorized)
		check(ep, "wrong-key", http.StatusUnauthorized)
	}
	for _, ep := range adminEP {
		check(ep, p01Key, http.StatusForbidden)
	}
	for _, ep := range participantEP {
		check(ep, adminKey, http.StatusForbidden)
	}
	for _, ep := range [][2]string{{"GET", "/api/namespaces"}, {"GET", "/api/webhook"}, {"GET", "/api/deliveries"}, {"GET", "/api/me"}, {"GET", "/api/journal"}} {
		check(ep, p01Key, http.StatusOK)
	}
	check([2]string{"GET", "/api/participants"}, adminKey, http.StatusOK)
	check([2]string{"GET", "/api/journal"}, adminKey, http.StatusOK)
	check([2]string{"GET", "/api/me"}, adminKey, http.StatusOK)
}

func TestAPIParticipantData(t *testing.T) {
	s := newTestServer(t)
	now := ts(time.Now())
	for _, ns := range []string{"p01-demo", "p02-demo"} {
		if _, err := s.db.Exec(`INSERT INTO namespaces VALUES (?, ?, 1, ?, ?, NULL)`, ns, ns[:3], now, now); err != nil {
			t.Fatal(err)
		}
	}
	w := apiCall(t, s, "GET", "/api/me", p01Key, "")
	if !strings.Contains(w.Body.String(), `"id":"p01"`) || !strings.Contains(w.Body.String(), "Jan Kowalski") {
		t.Errorf("me: %s", w.Body)
	}
	w = apiCall(t, s, "GET", "/api/namespaces", p01Key, "")
	if !strings.Contains(w.Body.String(), "p01-demo") || strings.Contains(w.Body.String(), "p02") ||
		!strings.Contains(w.Body.String(), `"app":{"state":"unknown"}`) {
		t.Errorf("namespaces: %s", w.Body)
	}

	for _, bad := range []string{`{"url":"http://example.com"}`, `{"url":"ftp://x"}`, `{"url":"nope"}`, `not json`} {
		s.cfg.AllowPrivateTargets = false
		if w := apiCall(t, s, "PUT", "/api/webhook-url", p01Key, bad); w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d", bad, w.Code)
		}
	}
	if w := apiCall(t, s, "PUT", "/api/webhook-url", p01Key, `{"url":"https://p01.example.com/hook"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	var wh map[string]string
	_ = json.Unmarshal(apiCall(t, s, "GET", "/api/webhook", p01Key, "").Body.Bytes(), &wh)
	if wh["url"] != "https://p01.example.com/hook" || wh["secret"] != testSecret {
		t.Errorf("webhook p01: %v", wh)
	}
	_ = json.Unmarshal(apiCall(t, s, "GET", "/api/webhook", p02Key, "").Body.Bytes(), &wh)
	if wh["url"] != "" {
		t.Errorf("p02 sees url %q", wh["url"])
	}

	if _, err := s.db.Exec(`INSERT INTO deliveries (participant_id, kind, headers, body, url, created_at)
		VALUES ('p02', 'github', '{}', x'', 'https://p02', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(t, s, "GET", "/api/deliveries", p01Key, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("p01 sees deliveries: %s", w.Body)
	}

	var ps []participantRow
	_ = json.Unmarshal(apiCall(t, s, "GET", "/api/participants", adminKey, "").Body.Bytes(), &ps)
	if len(ps) != 2 || ps[0].ID != "p01" || ps[0].WebhookURL == "" || len(ps[0].Namespaces) != 1 ||
		ps[1].LastDelivery == nil || ps[1].LastDelivery.URL != "https://p02" {
		t.Errorf("participants: %+v", ps)
	}
}

func TestAPIJournal(t *testing.T) {
	s := newTestServer(t)
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := base
	s.now = func() time.Time { return clock }

	create := func(body string) journalEntry {
		t.Helper()
		w := apiCall(t, s, "POST", "/api/journal", adminKey, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST %s: %d %s", body, w.Code, w.Body)
		}
		var e journalEntry
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return e
	}
	e1 := create(`{"namespace":"p01-demo","type":"agent","message":"scaled orders to 0","command":"kubectl -n p01-demo scale deploy/orders --replicas=0","undo":"kubectl -n p01-demo scale deploy/orders --replicas=1"}`)
	if e1.Participant != "p01" || e1.Status != "open" || e1.Undo == "" {
		t.Errorf("entry %+v", e1)
	}
	if e1.Type != "agent" {
		t.Errorf("type %q", e1.Type)
	}
	clock = base.Add(time.Hour)
	e2 := create(`{"namespace":"p02-demo","type":"manual","message":"x","status":"open"}`)

	for _, bad := range []string{`{"namespace":"p09-demo","type":"manual"}`, `{"namespace":"p01-demo","type":"other"}`,
		`{"namespace":"p01-demo","type":"drift"}`, `{"namespace":"p01-demo","type":"manual","status":"done"}`, `{`} {
		if w := apiCall(t, s, "POST", "/api/journal", adminKey, bad); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s: %d", bad, w.Code)
		}
	}

	list := func(key, q string) []journalEntry {
		t.Helper()
		w := apiCall(t, s, "GET", "/api/journal"+q, key, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", q, w.Code, w.Body)
		}
		var es []journalEntry
		_ = json.Unmarshal(w.Body.Bytes(), &es)
		return es
	}
	if es := list(adminKey, ""); len(es) != 2 {
		t.Errorf("admin all: %d", len(es))
	}
	if es := list(adminKey, "?participant=p02"); len(es) != 1 || es[0].ID != e2.ID {
		t.Errorf("admin p02: %+v", es)
	}
	if es := list(p01Key, ""); len(es) != 1 || es[0].ID != e1.ID {
		t.Errorf("p01 own: %+v", es)
	}
	if w := apiCall(t, s, "GET", "/api/journal?participant=p02", p01Key, ""); w.Code != http.StatusForbidden {
		t.Errorf("p01 filter p02: %d", w.Code)
	}
	since := base.Add(30 * time.Minute).Format(time.RFC3339)
	if es := list(adminKey, "?since="+since); len(es) != 1 || es[0].ID != e2.ID {
		t.Errorf("since: %+v", es)
	}
	if w := apiCall(t, s, "GET", "/api/journal?since=yesterday", adminKey, ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad since: %d", w.Code)
	}

	// A patch moves updated_at, so a poller with since sees the change.
	clock = base.Add(2 * time.Hour)
	w := apiCall(t, s, "PATCH", "/api/journal/1", adminKey, `{"status":"fixed"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"fixed"`) {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body)
	}
	since = base.Add(90 * time.Minute).Format(time.RFC3339)
	if es := list(adminKey, "?since="+since); len(es) != 1 || es[0].ID != e1.ID || es[0].Status != "fixed" {
		t.Errorf("since after patch: %+v", es)
	}
	for path, body := range map[string]string{"/api/journal/1": `{"status":"gone"}`, "/api/journal/2": `{}`,
		"/api/journal/3": `{"status":"fixed","message":"restored"}`} {
		if w := apiCall(t, s, "PATCH", path, adminKey, body); w.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s %s: %d", path, body, w.Code)
		}
	}
	if w := apiCall(t, s, "PATCH", "/api/journal/99", adminKey, `{"status":"fixed"}`); w.Code != http.StatusNotFound {
		t.Errorf("PATCH missing: %d", w.Code)
	}
}

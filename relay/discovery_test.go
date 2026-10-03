package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readList(t *testing.T, file string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	names, err := parseNamespaceList(body)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

type nsRow struct {
	Owner        string
	Connected    bool
	FirstSeen    string
	LastSeen     string
	Disconnected sql.NullString
}

func readNamespaces(t *testing.T, db *sql.DB) map[string]nsRow {
	t.Helper()
	rows, err := db.Query(`SELECT name, participant_id, connected, first_seen, last_seen, disconnected_at FROM namespaces`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]nsRow{}
	for rows.Next() {
		var n string
		var r nsRow
		if err := rows.Scan(&n, &r.Owner, &r.Connected, &r.FirstSeen, &r.LastSeen, &r.Disconnected); err != nil {
			t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

// logLines decodes the JSON log lines written since the last call.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		delete(m, "time")
		delete(m, "level")
		out = append(out, m)
	}
	buf.Reset()
	return out
}

func TestReconcile(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var buf bytes.Buffer
	d := newDiscovery(db, slog.New(slog.NewJSONHandler(&buf, nil)),
		[]participant{{ID: "p01"}, {ID: "p02"}})
	ctx := context.Background()
	t0 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	t1, t2, t3 := t0.Add(30*time.Second), t0.Add(time.Minute), t0.Add(90*time.Second)

	// 1. New: p01-demo and p02-demo connect; p-test and p99-x have no known owner.
	if err := d.reconcile(ctx, readList(t, "namespaces-1.json"), t0); err != nil {
		t.Fatal(err)
	}
	got := readNamespaces(t, db)
	want := map[string]nsRow{
		"p01-demo": {"p01", true, ts(t0), ts(t0), sql.NullString{}},
		"p02-demo": {"p02", true, ts(t0), ts(t0), sql.NullString{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after list 1:\n got %+v\nwant %+v", got, want)
	}
	wantLog := []map[string]any{
		{"msg": "namespace ignored: no known participant prefix", "namespace": "p-test"},
		{"msg": "namespace ignored: no known participant prefix", "namespace": "p99-x"},
		{"msg": "namespace connected", "namespace": "p01-demo", "participant": "p01", "state": "connected", "reason": "new"},
		{"msg": "namespace connected", "namespace": "p02-demo", "participant": "p02", "state": "connected", "reason": "new"},
	}
	if l := logLines(t, &buf); !reflect.DeepEqual(l, wantLog) {
		t.Errorf("log after list 1:\n got %v\nwant %v", l, wantLog)
	}

	// 2. Disappear: p01-demo lost its label. Ignored names are not logged again.
	if err := d.reconcile(ctx, readList(t, "namespaces-2.json"), t1); err != nil {
		t.Fatal(err)
	}
	got = readNamespaces(t, db)
	want = map[string]nsRow{
		"p01-demo": {"p01", false, ts(t0), ts(t0), sql.NullString{String: ts(t1), Valid: true}},
		"p02-demo": {"p02", true, ts(t0), ts(t1), sql.NullString{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after list 2:\n got %+v\nwant %+v", got, want)
	}
	wantLog = []map[string]any{
		{"msg": "namespace disconnected", "namespace": "p01-demo", "participant": "p01", "state": "disconnected", "reason": "not listed"},
	}
	if l := logLines(t, &buf); !reflect.DeepEqual(l, wantLog) {
		t.Errorf("log after list 2:\n got %v\nwant %v", l, wantLog)
	}

	// Still missing: no new change, disconnected_at is kept.
	if err := d.reconcile(ctx, readList(t, "namespaces-2.json"), t2); err != nil {
		t.Fatal(err)
	}
	if r := readNamespaces(t, db)["p01-demo"]; r.Disconnected.String != ts(t1) {
		t.Errorf("disconnected_at = %v, want %s", r.Disconnected, ts(t1))
	}
	if l := logLines(t, &buf); len(l) != 0 {
		t.Errorf("log with no change: %v", l)
	}

	// 3. Reappear: p01-demo is connected again with its first_seen kept.
	if err := d.reconcile(ctx, readList(t, "namespaces-3.json"), t3); err != nil {
		t.Fatal(err)
	}
	got = readNamespaces(t, db)
	want = map[string]nsRow{
		"p01-demo": {"p01", true, ts(t0), ts(t3), sql.NullString{}},
		"p02-demo": {"p02", true, ts(t0), ts(t3), sql.NullString{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after list 3:\n got %+v\nwant %+v", got, want)
	}
	wantLog = []map[string]any{
		{"msg": "namespace connected", "namespace": "p01-demo", "participant": "p01", "state": "connected", "reason": "reappeared"},
	}
	if l := logLines(t, &buf); !reflect.DeepEqual(l, wantLog) {
		t.Errorf("log after list 3:\n got %v\nwant %v", l, wantLog)
	}

	// An empty list disconnects everything (all labels removed).
	if err := d.reconcile(ctx, nil, t3.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for n, r := range readNamespaces(t, db) {
		if r.Connected {
			t.Errorf("%s still connected after an empty list", n)
		}
	}
}

func TestParseNamespaceListRejectsStatus(t *testing.T) {
	if _, err := parseNamespaceList([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":403}`)); err == nil {
		t.Error("Status body: want error")
	}
}

func TestKubeClientOverride(t *testing.T) {
	body, err := os.ReadFile("testdata/namespaces-3.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotSelector, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSelector, gotAuth = r.URL.Query().Get("labelSelector"), r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/namespaces" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	t.Setenv("KUBE_API", srv.URL+"/")
	t.Setenv("KUBE_TOKEN", "tok")
	k, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	names, err := k.namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"p-test", "p01-demo", "p02-demo"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if gotSelector != targetLabel || gotAuth != "Bearer tok" {
		t.Errorf("selector %q, auth %q", gotSelector, gotAuth)
	}

	// Without KUBE_TOKEN (kubectl proxy) no Authorization header is sent.
	t.Setenv("KUBE_TOKEN", "")
	k, _ = newKubeClient()
	if _, err := k.namespaces(context.Background()); err != nil || gotAuth != "" {
		t.Errorf("proxy mode: err %v, auth %q", err, gotAuth)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSecret = "test-secret" // the secret of the temporary hook that recorded testdata/
	adminKey   = "admin-key-0000"
	p01Key     = "00000000-0000-4000-8000-000000000001"
	p02Key     = "00000000-0000-4000-8000-000000000002"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config{
		Participants: []participant{
			{ID: "p01", Name: "Jan Kowalski", APIKey: p01Key},
			{ID: "p02", Name: "Anna Nowak", APIKey: p02Key},
		},
		AdminKey:            adminKey,
		WebhookSecret:       testSecret,
		TargetDomain:        "lab.patoarchitekci.io",
		AllowPrivateTargets: true, // receivers are httptest servers on 127.0.0.1
	}
	s := newServer(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(s.wait)
	return s
}

func (s *server) serve(r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// receiver records the requests it gets.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []captured
}

type captured struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, captured{r.Header.Clone(), b})
		rc.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) got() []captured {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]captured(nil), rc.reqs...)
}

// recorded returns a body and headers GitHub sent to the temporary hook.
func recorded(t *testing.T, name string) ([]byte, map[string]string) {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	hb, err := os.ReadFile("testdata/" + name + ".headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]string
	if err := json.Unmarshal(hb, &h); err != nil {
		t.Fatal(err)
	}
	return body, h
}

func githubRequest(body []byte, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func setURL(t *testing.T, s *server, pid, u string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO webhook_urls VALUES (?, ?, 'x')`, pid, u); err != nil {
		t.Fatal(err)
	}
}

func lastEventOutcome(t *testing.T, s *server) (outcome, detail string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT outcome, COALESCE(detail, '') FROM github_events ORDER BY id DESC LIMIT 1`).Scan(&outcome, &detail); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRecordedSignatures(t *testing.T) {
	for _, n := range []string{"ping", "deployment_status_success_p01", "deployment_status_failure_p01", "deployment_status_in_progress_p01", "deployment_status_success_p02"} {
		body, h := recorded(t, n)
		if !validSignature(testSecret, body, h["X-Hub-Signature-256"]) {
			t.Errorf("%s: recorded signature does not verify", n)
		}
		if got := signSHA1(testSecret, body); got != h["X-Hub-Signature"] {
			t.Errorf("%s: sha1 = %s, want %s", n, got, h["X-Hub-Signature"])
		}
	}
}

func TestWebhookHMAC(t *testing.T) {
	s := newTestServer(t)
	body, h := recorded(t, "ping")

	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusOK {
		t.Fatalf("good signature: %d %s", w.Code, w.Body)
	}

	bad := map[string]string{}
	for k, v := range h {
		bad[k] = v
	}
	bad["X-Hub-Signature-256"] = signSHA256("other-secret", body)
	if w := s.serve(githubRequest(body, bad)); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", w.Code)
	}
	if o, d := lastEventOutcome(t, s); o != "bad_signature" || !strings.Contains(d, "mismatch") {
		t.Errorf("bad signature logged as %q %q", o, d)
	}

	delete(bad, "X-Hub-Signature-256")
	if w := s.serve(githubRequest(body, bad)); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: %d", w.Code)
	}
	if o, d := lastEventOutcome(t, s); o != "bad_signature" || !strings.Contains(d, "missing") {
		t.Errorf("missing signature logged as %q %q", o, d)
	}

	// A signed body with one byte changed is refused.
	tampered := bytes.Replace(body, []byte("zen"), []byte("Zen"), 1)
	if w := s.serve(githubRequest(tampered, h)); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", w.Code)
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	s := newTestServer(t)
	body := bytes.Repeat([]byte("a"), maxWebhookBody+1)
	h := map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": signSHA256(testSecret, body)}
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", w.Code)
	}
}

func TestWebhookRoutesToOwnerOnly(t *testing.T) {
	s := newTestServer(t)
	r1, r2 := newReceiver(t), newReceiver(t)
	setURL(t, s, "p01", r1.URL+"/hook")
	setURL(t, s, "p02", r2.URL+"/hook")

	body, h := recorded(t, "deployment_status_success_p01")
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusAccepted {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	s.wait()

	got := r1.got()
	if len(got) != 1 {
		t.Fatalf("owner got %d requests, want 1", len(got))
	}
	if !bytes.Equal(got[0].body, body) {
		t.Error("owner body differs from the GitHub body")
	}
	for _, k := range forwardHeaders {
		if g := got[0].header.Get(k); g != h[http.CanonicalHeaderKey(k)] && g != h[k] {
			t.Errorf("header %s = %q, want %q", k, g, h[k])
		}
	}
	if !validSignature(testSecret, got[0].body, got[0].header.Get("X-Hub-Signature-256")) {
		t.Error("forwarded signature does not verify with the shared secret")
	}
	if n := len(r2.got()); n != 0 {
		t.Errorf("other participant got %d requests", n)
	}

	var pid, kind, gd string
	var code int
	if err := s.db.QueryRow(`SELECT participant_id, kind, github_delivery, status_code FROM deliveries`).Scan(&pid, &kind, &gd, &code); err != nil {
		t.Fatal(err)
	}
	if pid != "p01" || kind != "github" || gd != h["X-Github-Delivery"] || code != http.StatusNoContent {
		t.Errorf("delivery = %s %s %s %d", pid, kind, gd, code)
	}

	e, err := scanJournal(s.db.QueryRow(`SELECT ` + journalCols + ` FROM journal`))
	if err != nil {
		t.Fatal(err)
	}
	want := journalEntry{ID: e.ID, Participant: "p01", Namespace: "p01-demo", Type: "deploy", Status: "success",
		Version: "1.0.3", RunURL: "https://github.com/ProtopiaTech-Labs/ai-coding-devops-lab/actions/runs/37144955898",
		RunName: "deploy p01-demo 1.0.3 break=none", CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	if e != want {
		t.Errorf("journal = %+v\nwant      %+v", e, want)
	}
}

func TestWebhookFailureAndInProgress(t *testing.T) {
	s := newTestServer(t)
	r1 := newReceiver(t)
	setURL(t, s, "p01", r1.URL)

	// in_progress has no environment_url: routed by the run name, no journal entry.
	body, h := recorded(t, "deployment_status_in_progress_p01")
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusAccepted {
		t.Fatalf("in_progress: %d", w.Code)
	}
	body, h = recorded(t, "deployment_status_failure_p01")
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusAccepted {
		t.Fatalf("failure: %d", w.Code)
	}
	s.wait()
	if n := len(r1.got()); n != 2 {
		t.Errorf("owner got %d requests, want 2", n)
	}
	var status, version string
	var n int
	if err := s.db.QueryRow(`SELECT count(*), MAX(status), MAX(version) FROM journal`).Scan(&n, &status, &version); err != nil {
		t.Fatal(err)
	}
	if n != 1 || status != "failure" || version != "9.9.9" {
		t.Errorf("journal: %d entries, %s %s; want 1 failure 9.9.9", n, status, version)
	}
}

func TestWebhookUnroutedAndNoURL(t *testing.T) {
	s := newTestServer(t)
	r1, r2 := newReceiver(t), newReceiver(t)
	setURL(t, s, "p01", r1.URL)
	setURL(t, s, "p02", r2.URL)

	body, _ := recorded(t, "deployment_status_success_p01")
	body = bytes.ReplaceAll(body, []byte("p01-demo"), []byte("p09-demo"))
	h := map[string]string{"X-GitHub-Event": "deployment_status", "X-Hub-Signature-256": signSHA256(testSecret, body)}
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	s.wait()
	if len(r1.got())+len(r2.got()) != 0 {
		t.Error("unrouted event was forwarded")
	}
	if o, _ := lastEventOutcome(t, s); o != "unrouted" {
		t.Errorf("outcome %q", o)
	}

	// p02 without a URL: nothing sent, the journal entry is still written.
	if _, err := s.db.Exec(`DELETE FROM webhook_urls WHERE participant_id = 'p02'`); err != nil {
		t.Fatal(err)
	}
	body, h = recorded(t, "deployment_status_success_p02")
	if w := s.serve(githubRequest(body, h)); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	if o, _ := lastEventOutcome(t, s); o != "no_url" {
		t.Errorf("outcome %q", o)
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM journal WHERE participant_id = 'p02'`).Scan(&n)
	if n != 1 {
		t.Errorf("p02 journal entries = %d", n)
	}
}

func TestReplayResendsOriginal(t *testing.T) {
	s := newTestServer(t)
	r1 := newReceiver(t)
	setURL(t, s, "p01", r1.URL)
	body, h := recorded(t, "deployment_status_success_p01")
	s.serve(githubRequest(body, h))
	s.wait()

	req := httptest.NewRequest(http.MethodPost, "/api/webhook/replay?last=5", nil)
	req.Header.Set("X-Api-Key", p01Key)
	w := s.serve(req)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"queued":1`) {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	s.wait()
	got := r1.got()
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	if !bytes.Equal(got[1].body, body) || got[1].header.Get("X-Hub-Signature-256") != h["X-Hub-Signature-256"] ||
		got[1].header.Get("X-GitHub-Delivery") != h["X-Github-Delivery"] {
		t.Error("replay differs from the original body or headers")
	}
	var kind string
	_ = s.db.QueryRow(`SELECT kind FROM deliveries ORDER BY id DESC LIMIT 1`).Scan(&kind)
	if kind != "replay" {
		t.Errorf("kind %q", kind)
	}

	for _, q := range []string{"0", "21", "x"} {
		req := httptest.NewRequest(http.MethodPost, "/api/webhook/replay?last="+q, nil)
		req.Header.Set("X-Api-Key", p01Key)
		if w := s.serve(req); w.Code != http.StatusBadRequest {
			t.Errorf("last=%s: %d", q, w.Code)
		}
	}
}

func TestTestEventSigned(t *testing.T) {
	s := newTestServer(t)
	r1 := newReceiver(t)
	setURL(t, s, "p01", r1.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/test", nil)
	req.Header.Set("X-Api-Key", p01Key)
	w := s.serve(req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var d delivery
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Kind != "test" || d.StatusCode != http.StatusNoContent || d.Participant != "p01" {
		t.Errorf("delivery %+v", d)
	}
	got := r1.got()
	if len(got) != 1 {
		t.Fatalf("got %d requests", len(got))
	}
	if !validSignature(testSecret, got[0].body, got[0].header.Get("X-Hub-Signature-256")) {
		t.Error("test event signature does not verify with the shared secret")
	}
	if got[0].header.Get("X-GitHub-Event") != "ping" || got[0].header.Get("X-Hub-Signature") != signSHA1(testSecret, got[0].body) {
		t.Errorf("headers %v", got[0].header)
	}
}

func TestParseRunName(t *testing.T) {
	cases := map[string]runName{
		"deploy p01-demo 1.0.3 break=none":  {"p01-demo", "1.0.3", "none"},
		"deploy p02-shop 1.0.12 break=oom":  {"p02-shop", "1.0.12", "oom"},
		" deploy p01-x 2.0.0 break=crash\n": {"p01-x", "2.0.0", "crash"},
	}
	for in, want := range cases {
		if got, ok := parseRunName(in); !ok || got != want {
			t.Errorf("%q = %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "deploy", "build 1.0.3", "deploy p01-demo 1.0.3", "deploy p01 demo 1.0.3 break=none"} {
		if _, ok := parseRunName(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestNamespaceFromURL(t *testing.T) {
	d := "lab.patoarchitekci.io"
	cases := map[string]string{
		"https://p01-demo.lab.patoarchitekci.io/version": "p01-demo",
		"https://P01-Demo.LAB.patoarchitekci.io":         "p01-demo",
		"https://a.p01-demo.lab.patoarchitekci.io/":      "",
		"https://lab.patoarchitekci.io/":                 "",
		"https://p01-demo.evil.io/lab.patoarchitekci.io": "",
		"":          "",
		"://broken": "",
		"p01-demo":  "",
		"https://p01-demo.lab.patoarchitekci.io.evil.io": "",
	}
	for in, want := range cases {
		if got := namespaceFromURL(in, d); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func TestBlockedAddr(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"fe80::1", "fc00::1", "fd12::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1"}
	for _, a := range blocked {
		if !blockedAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s not blocked", a)
		}
	}
	for _, a := range []string{"140.82.112.3", "1.1.1.1", "2606:4700::1111", "172.32.0.1"} {
		if blockedAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s blocked", a)
		}
	}
}

func TestOutboundClientGuards(t *testing.T) {
	r := newReceiver(t) // on 127.0.0.1
	c := newOutboundClient(2*time.Second, false)
	if _, err := send(context.Background(), c, r.URL, nil, []byte("{}")); !errors.Is(err, errBlockedTarget) {
		t.Errorf("loopback target: err = %v, want errBlockedTarget", err)
	}
	if n := len(r.got()); n != 0 {
		t.Errorf("blocked target got %d requests", n)
	}

	// Redirects are returned, not followed.
	redir := httptest.NewServer(http.RedirectHandler(r.URL, http.StatusFound))
	defer redir.Close()
	code, err := send(context.Background(), newOutboundClient(2*time.Second, true), redir.URL, nil, []byte("{}"))
	if err != nil || code != http.StatusFound || len(r.got()) != 0 {
		t.Errorf("redirect: code %d err %v, followed %d", code, err, len(r.got()))
	}

	if err := validWebhookURL("http://example.com/hook", false); err == nil {
		t.Error("http:// accepted without AllowPrivateTargets")
	}
	for _, u := range []string{"https://example.com/hook", "https://agent.p01.example:8443/x"} {
		if err := validWebhookURL(u, false); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"ftp://example.com", "https://", "example.com", "https://u:p@example.com/"} {
		if err := validWebhookURL(u, true); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

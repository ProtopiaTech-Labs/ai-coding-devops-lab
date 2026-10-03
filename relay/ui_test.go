package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type uiEnv struct {
	s   *server
	u   *ui
	mux *http.ServeMux
}

func newUIEnv(t *testing.T) *uiEnv {
	t.Helper()
	s := newTestServer(t)
	u, err := newUI(s, func(_ context.Context, ns string) workload {
		return workload{Version: "1.0.7", Ready: 2, Total: 3, Problem: "CrashLoopBackOff"}
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	u.routes(mux)
	return &uiEnv{s, u, mux}
}

// do sends a request as a same-origin browser with the given session key.
func (e *uiEnv) do(method, path, key string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "http://relay.test"+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if key != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: key})
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func TestUILogin(t *testing.T) {
	e := newUIEnv(t)

	w := e.do("POST", "/login", "", url.Values{"key": {"wrong"}})
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 || !strings.Contains(w.Body.String(), "Unknown API key") {
		t.Fatalf("wrong key: %d cookies=%v", w.Code, w.Result().Cookies())
	}

	w = e.do("POST", "/login", "", url.Values{"key": {p01Key}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/namespaces" {
		t.Fatalf("participant login: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != sessionCookie || c[0].Value != p01Key || !c[0].HttpOnly ||
		c[0].SameSite != http.SameSiteStrictMode || c[0].Secure {
		t.Fatalf("cookie: %+v", c)
	}

	w = e.do("POST", "/login", "", url.Values{"key": {adminKey}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin" {
		t.Fatalf("admin login: %d %q", w.Code, w.Header().Get("Location"))
	}

	// Without RELAY_ALLOW_PRIVATE_TARGETS the cookie is Secure.
	e.s.cfg.AllowPrivateTargets = false
	u, err := newUI(e.s, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	u.routes(mux)
	r := httptest.NewRequest("POST", "/login", strings.NewReader("key="+p01Key))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("secure cookie: %+v", c)
	}
}

func TestUILogout(t *testing.T) {
	e := newUIEnv(t)
	w := e.do("POST", "/logout", p01Key, url.Values{})
	c := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(c) != 1 || c[0].Name != sessionCookie || c[0].MaxAge >= 0 || c[0].Value != "" {
		t.Fatalf("logout: %d %+v", w.Code, c)
	}
}

func TestUIAccess(t *testing.T) {
	e := newUIEnv(t)
	for _, path := range []string{"/admin", "/admin/journal", "/admin/github", "/admin/participants/p01"} {
		if w := e.do("GET", path, p01Key, nil); w.Code != http.StatusForbidden {
			t.Errorf("participant %s: %d", path, w.Code)
		}
		if w := e.do("GET", path, "", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("anonymous %s: %d", path, w.Code)
		}
		if w := e.do("GET", path, adminKey, nil); w.Code != http.StatusOK {
			t.Errorf("admin %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/namespaces", "/webhook", "/journal", "/webhook/deliveries"} {
		if w := e.do("GET", path, adminKey, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin" {
			t.Errorf("admin %s: %d", path, w.Code)
		}
		if w := e.do("GET", path, "stale-key", nil); w.Code != http.StatusSeeOther {
			t.Errorf("unknown key %s: %d", path, w.Code)
		}
		if w := e.do("GET", path, p01Key, nil); w.Code != http.StatusOK {
			t.Errorf("participant %s: %d", path, w.Code)
		}
	}
	// An expired session in an htmx poll redirects the whole page.
	if w := e.do("GET", "/webhook/deliveries", "", nil, "HX-Request", "true"); w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx without session: %d %v", w.Code, w.Header())
	}
	if w := e.do("GET", "/admin/participants/p99", adminKey, nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown participant: %d", w.Code)
	}
}

func TestUICSRF(t *testing.T) {
	e := newUIEnv(t)
	form := url.Values{"url": {"https://evil.example/hook"}}
	for _, h := range [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "", "Origin", "https://evil.example"},
	} {
		if w := e.do("POST", "/webhook/url", p01Key, form, h...); w.Code != http.StatusForbidden {
			t.Errorf("%v: %d", h, w.Code)
		}
	}
	if w := e.do("POST", "/login", "", url.Values{"key": {p01Key}}, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Errorf("cross-site login: %d", w.Code)
	}
	if u, _, _ := e.s.webhookSetting(context.Background(), "p01"); u != "" {
		t.Errorf("cross-site write stored %q", u)
	}
}

func TestUIWebhookActions(t *testing.T) {
	e := newUIEnv(t)
	rc := newReceiver(t)

	w := e.do("POST", "/webhook/url", p01Key, url.Values{"url": {"ftp://x"}}, "HX-Request", "true")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "URL must start with https://") {
		t.Fatalf("invalid url: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/webhook/test", p01Key, url.Values{}, "HX-Request", "true")
	if !strings.Contains(w.Body.String(), "set a webhook URL first") {
		t.Fatalf("test without url: %s", w.Body)
	}

	w = e.do("POST", "/webhook/url", p01Key, url.Values{"url": {rc.URL + "/hook"}}, "HX-Request", "true")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Saved.") {
		t.Fatalf("save url: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/webhook/test", p01Key, url.Values{}, "HX-Request", "true")
	body := w.Body.String()
	if !strings.Contains(body, "Test event sent: HTTP 204") || !strings.Contains(body, `hx-swap-oob="innerHTML"`) ||
		!strings.Contains(body, ">ping<") {
		t.Fatalf("test event: %s", body)
	}
	if len(rc.got()) != 1 {
		t.Fatalf("receiver got %d requests", len(rc.got()))
	}
	w = e.do("POST", "/webhook/replay", p01Key, url.Values{"last": {"5"}}, "HX-Request", "true")
	if !strings.Contains(w.Body.String(), "No GitHub deliveries to replay yet.") {
		t.Fatalf("replay: %s", w.Body)
	}
	w = e.do("POST", "/webhook/replay", p01Key, url.Values{"last": {"50"}}, "HX-Request", "true")
	if !strings.Contains(w.Body.String(), "last must be 1..20") {
		t.Fatalf("replay 50: %s", w.Body)
	}
}

func TestUIRendersSampleData(t *testing.T) {
	e := newUIEnv(t)
	now := ts(time.Now())
	old := ts(time.Now().Add(-time.Hour))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO namespaces VALUES ('p01-demo', 'p01', 1, ?, ?, NULL), ('p01-old', 'p01', 0, ?, ?, ?), ('p02-demo', 'p02', 1, ?, ?, NULL)`,
		now, now, now, now, now, now, now)
	exec(`INSERT INTO journal (participant_id, namespace, type, status, version, run_url, run_name, message, created_at, updated_at)
		VALUES ('p01', 'p01-demo', 'deploy', 'success', '1.0.7', 'https://github.com/x/actions/runs/1', 'deploy p01-demo 1.0.7 break=none', '<script>alert(1)</script>', ?, ?),
		       ('p02', 'p02-demo', 'manual', 'open', NULL, NULL, NULL, 'scaled to zero', ?, ?)`, now, now, old, old)
	exec(`INSERT INTO deliveries (participant_id, kind, event, github_delivery, headers, body, url, status_code, duration_ms, created_at)
		VALUES ('p01', 'github', 'deployment_status', 'gd-1', '{}', x'', 'https://p01.example/hook', 502, 31, ?)`, now)
	exec(`INSERT INTO webhook_urls VALUES ('p01', 'https://p01.example/hook', ?)`, now)
	exec(`INSERT INTO github_events (event, github_delivery, outcome, detail, created_at) VALUES ('ping', 'gd-x', 'bad_signature', 'signature mismatch from 1.2.3.4', ?)`, now)

	check := func(path, key string, want ...string) string {
		t.Helper()
		w := e.do("GET", path, key, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		b := w.Body.String()
		for _, s := range want {
			if !strings.Contains(b, s) {
				t.Errorf("%s: missing %q", path, s)
			}
		}
		if strings.Contains(b, "<script>alert(1)") {
			t.Errorf("%s: unescaped journal message", path)
		}
		return b
	}
	b := check("/namespaces", p01Key, "p01 · Jan Kowalski", "p01-demo", "connected", "1.0.7", "2/3 ready",
		"CrashLoopBackOff", "https://p01-demo.lab.patoarchitekci.io/version", "p01-old", "disconnected")
	if strings.Contains(b, "p02-demo") {
		t.Error("p01 sees p02-demo")
	}
	check("/webhook", p01Key, testSecret, `data-copy="secret"`, "https://p01.example/hook", "gd-1", ">502<",
		`hx-trigger="every 5s"`)
	b = check("/journal", p01Key, "&lt;script&gt;alert(1)&lt;/script&gt;", "deploy p01-demo 1.0.7 break=none")
	if strings.Contains(b, "scaled to zero") {
		t.Error("p01 sees p02 journal")
	}
	b = check("/admin", adminKey, "Jan Kowalski", "Anna Nowak", "https://p01.example/hook", ">502<", "1h ago")
	if !strings.Contains(b, `data-variant="destructive">1h ago`) {
		t.Error("stale journal entry not red")
	}
	check("/admin/participants/p01", adminKey, "p01 · Jan Kowalski", "p01-demo", "gd-1", "deploy p01-demo 1.0.7 break=none")
	b = check("/admin/journal?type=manual", adminKey, "scaled to zero")
	if strings.Contains(b, "deploy p01-demo 1.0.7 break=none") {
		t.Error("type filter ignored")
	}
	b = check("/admin/journal?participant=p01&status=success", adminKey, "deploy p01-demo")
	if strings.Contains(b, "scaled to zero") {
		t.Error("participant filter ignored")
	}
	check("/admin/github", adminKey, `class="row-error"`, "bad_signature", "signature mismatch")
	check("/login", "", `name="key"`)
}

func TestWorkloadRead(t *testing.T) {
	deploy := `{"spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/protopiatech-labs/shop:1.0.2"}]}}}}`
	pods := `{"items":[
		{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
		{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],
		  "containerStatuses":[{"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
		{"status":{"phase":"Succeeded"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/p01-demo/deployments/orders":
			w.Write([]byte(deploy))
		case "/api/v1/namespaces/p01-demo/pods", "/api/v1/namespaces/p01-empty/pods":
			w.Write([]byte(pods))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KUBE_API", srv.URL)
	k, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	got := k.workload(context.Background(), "p01-demo")
	if want := (workload{Version: "1.0.2", Ready: 1, Total: 2, Problem: "CrashLoopBackOff"}); got != want {
		t.Errorf("p01-demo: %+v, want %+v", got, want)
	}
	if got := k.workload(context.Background(), "p01-empty"); got.Version != "" || got.Err != "" || got.Total != 2 {
		t.Errorf("no deployment: %+v", got)
	}
	srv.Close()
	if got := k.workload(context.Background(), "p01-demo"); got.Err == "" {
		t.Errorf("API down: %+v", got)
	}

	for img, want := range map[string]string{"shop:1.0.2": "1.0.2", "localhost:5000/shop": "latest", "shop:1.0.3@sha256:ab": "1.0.3"} {
		b := `{"spec":{"template":{"spec":{"containers":[{"image":"` + img + `"}]}}}}`
		if got := parseDeploymentVersion([]byte(b)); got != want {
			t.Errorf("%s: %q, want %q", img, got, want)
		}
	}
}

func TestWorkloadCache(t *testing.T) {
	calls := 0
	c := newWorkloadCache(func(context.Context, string) workload { calls++; return workload{Ready: calls} }, 15*time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.get(context.Background(), "a")
	c.get(context.Background(), "a")
	if calls != 1 {
		t.Fatalf("calls = %d within ttl", calls)
	}
	now = now.Add(16 * time.Second)
	if w := c.get(context.Background(), "a"); calls != 2 || w.Ready != 2 {
		t.Fatalf("after ttl: calls %d, %+v", calls, w)
	}
}

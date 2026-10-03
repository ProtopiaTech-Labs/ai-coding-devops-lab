package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The UI: server-rendered html/template pages with Basecoat and htmx, both
// vendored in ui/static. The session cookie holds the API key; every request
// resolves it again, so a participants.txt change takes effect on restart.

//go:embed ui/templates/*.html ui/static/*
var uiFiles embed.FS

const sessionCookie = "relay_key"

type ui struct {
	s        *server
	workload func(ctx context.Context, ns string) workload
	pages    map[string]*template.Template
	csrf     *http.CrossOriginProtection
	secure   bool   // Secure cookie; off only with RELAY_ALLOW_PRIVATE_TARGETS (local http)
	assets   string // hash of ui/static, appended to asset URLs
}

// view is the data of every page and fragment.
type view struct {
	Title   string
	Nav     string
	Admin   bool
	Who     participant
	Version string
	Error   string
	Notice  string
	Domain  string
	Assets  string // cache buster for /static links

	// participant screens and admin detail
	Participant participant
	Namespaces  []nsView
	WebhookURL  string
	WebhookAt   string
	Secret      string
	Deliveries  []delivery
	Journal     []journalEntry

	// admin screens
	Overview []participantRow
	Events   []githubEvent
	Filter   journalFilter
	IDs      []participant
}

type nsView struct {
	namespaceRow
	workload
}

func newUI(s *server, workloadOf func(context.Context, string) workload) (*ui, error) {
	u := &ui{
		s:        s,
		workload: workloadOf,
		pages:    map[string]*template.Template{},
		csrf:     http.NewCrossOriginProtection(),
		secure:   !s.cfg.AllowPrivateTargets,
	}
	h := sha256.New()
	_ = fs.WalkDir(uiFiles, "ui/static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := uiFiles.ReadFile(path)
			h.Write(b)
		}
		return nil
	})
	u.assets = hex.EncodeToString(h.Sum(nil))[:12]
	u.csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	funcs := template.FuncMap{
		"clock": func(t string) string { return fmtTime(t, "01-02 15:04:05") },
		"age":   func(t string) string { return age(s.now(), t) },
		"stale": func(t string) bool { return olderThan(s.now(), t, 10*time.Minute) },
		"codeVariant": func(code int) string {
			switch {
			case code >= 200 && code < 300:
				return "primary"
			case code == 0 || code >= 500:
				return "destructive"
			default:
				return "secondary"
			}
		},
		"statusVariant": func(st string) string {
			switch st {
			case "success", "fixed":
				return "primary"
			case "failure", "error", "open":
				return "destructive"
			default:
				return "secondary"
			}
		},
	}
	base, err := template.New("").Funcs(funcs).ParseFS(uiFiles, "ui/templates/layout.html", "ui/templates/parts.html")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"login", "namespaces", "webhook", "journal", "forbidden",
		"admin", "admin_participant", "admin_journal", "admin_github"} {
		t, err := template.Must(base.Clone()).ParseFS(uiFiles, "ui/templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		u.pages[name] = t
	}
	u.pages["parts"] = base
	return u, nil
}

func (u *ui) routes(mux *http.ServeMux) {
	static, _ := fs.Sub(uiFiles, "ui/static")
	fileServer := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /{$}", u.handleRoot)
	mux.HandleFunc("GET /login", u.handleLoginForm)
	mux.Handle("POST /login", u.csrf.Handler(http.HandlerFunc(u.handleLogin)))
	mux.Handle("POST /logout", u.csrf.Handler(http.HandlerFunc(u.handleLogout)))

	mux.HandleFunc("GET /namespaces", u.participant(u.handleNamespaces))
	mux.HandleFunc("GET /webhook", u.participant(u.handleWebhook))
	mux.HandleFunc("GET /webhook/deliveries", u.participant(u.handleDeliveries))
	mux.Handle("POST /webhook/url", u.csrf.Handler(u.participant(u.handleSetURL)))
	mux.Handle("POST /webhook/test", u.csrf.Handler(u.participant(u.handleTestEvent)))
	mux.Handle("POST /webhook/replay", u.csrf.Handler(u.participant(u.handleReplayUI)))
	mux.HandleFunc("GET /journal", u.participant(u.handleJournal))

	mux.HandleFunc("GET /admin", u.admin(u.handleAdmin))
	mux.HandleFunc("GET /admin/participants/{id}", u.admin(u.handleAdminParticipant))
	mux.HandleFunc("GET /admin/journal", u.admin(u.handleAdminJournal))
	mux.HandleFunc("GET /admin/github", u.admin(u.handleAdminGitHub))
}

// session resolves the cookie with the same key check as X-Api-Key.
func (u *ui) session(r *http.Request) (p participant, admin, ok bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return participant{}, false, false
	}
	return u.s.callerKey(c.Value)
}

// toLogin sends a browser to the login page; htmx gets HX-Redirect.
func toLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// participant: no session → login; admin → admin overview.
func (u *ui) participant(h func(http.ResponseWriter, *http.Request, participant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, admin, ok := u.session(r)
		switch {
		case !ok:
			toLogin(w, r)
		case admin:
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
		default:
			h(w, r, p)
		}
	}
}

// admin: no session → login; participant → 403.
func (u *ui) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, admin, ok := u.session(r)
		switch {
		case !ok:
			toLogin(w, r)
		case !admin:
			u.render(w, http.StatusForbidden, "forbidden", "layout", view{Title: "Forbidden", Who: p})
		default:
			h(w, r)
		}
	}
}

// render executes a template into a buffer first, so a template error is a
// clean 500 and not half a page.
func (u *ui) render(w http.ResponseWriter, status int, page, tmpl string, v view) {
	v.Version = version
	v.Domain = u.s.cfg.TargetDomain
	v.Assets = u.assets
	var buf bytes.Buffer
	if err := u.pages[page].ExecuteTemplate(&buf, tmpl, v); err != nil {
		u.s.log.Error("ui: render", "page", page, "template", tmpl, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (u *ui) fail(w http.ResponseWriter, err error) {
	u.s.log.Error("ui: store", "err", err)
	http.Error(w, "database error", http.StatusInternalServerError)
}

func (u *ui) handleRoot(w http.ResponseWriter, r *http.Request) {
	_, admin, ok := u.session(r)
	switch {
	case !ok:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case admin:
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/namespaces", http.StatusSeeOther)
	}
}

func (u *ui) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	u.render(w, http.StatusOK, "login", "layout", view{Title: "Log in"})
}

func (u *ui) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	key := strings.TrimSpace(r.PostFormValue("key"))
	_, admin, ok := u.s.callerKey(key)
	if !ok {
		u.s.log.Warn("ui: login refused", "remote", r.RemoteAddr)
		u.render(w, http.StatusUnauthorized, "login", "layout", view{Title: "Log in", Error: "Unknown API key."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: key, Path: "/", MaxAge: 12 * 3600,
		HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteStrictMode,
	})
	if admin {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/namespaces", http.StatusSeeOther)
}

func (u *ui) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// namespaceViews adds the cluster state to the connected namespaces.
func (u *ui) namespaceViews(ctx context.Context, pid string) ([]nsView, error) {
	rows, err := u.s.listNamespaces(ctx, pid)
	if err != nil {
		return nil, err
	}
	out := make([]nsView, 0, len(rows))
	for _, n := range rows {
		v := nsView{namespaceRow: n}
		if n.Connected {
			v.workload = u.workload(ctx, n.Name)
		}
		out = append(out, v)
	}
	return out, nil
}

func (u *ui) handleNamespaces(w http.ResponseWriter, r *http.Request, p participant) {
	ns, err := u.namespaceViews(r.Context(), p.ID)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "namespaces", "layout", view{Title: "Namespaces", Nav: "namespaces", Who: p, Namespaces: ns})
}

func (u *ui) webhookView(ctx context.Context, p participant) (view, error) {
	v := view{Title: "Webhook", Nav: "webhook", Who: p, Secret: u.s.cfg.WebhookSecret}
	var err error
	if v.WebhookURL, v.WebhookAt, err = u.s.webhookSetting(ctx, p.ID); err != nil {
		return v, err
	}
	v.Deliveries, err = u.s.deliveries(ctx, p.ID, 50)
	return v, err
}

func (u *ui) handleWebhook(w http.ResponseWriter, r *http.Request, p participant) {
	v, err := u.webhookView(r.Context(), p)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "webhook", "layout", v)
}

func (u *ui) handleDeliveries(w http.ResponseWriter, r *http.Request, p participant) {
	ds, err := u.s.deliveries(r.Context(), p.ID, 50)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "parts", "deliveries", view{Deliveries: ds})
}

// handleSetURL re-renders the URL form with the stored value or the error.
func (u *ui) handleSetURL(w http.ResponseWriter, r *http.Request, p participant) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw := r.PostFormValue("url")
	got, at, err := u.s.setWebhookURL(r.Context(), p.ID, raw)
	var ie inputError
	switch {
	case errors.As(err, &ie):
		u.render(w, http.StatusUnprocessableEntity, "parts", "url-form", view{WebhookURL: raw, Error: ie.msg})
	case err != nil:
		u.fail(w, err)
	case got == "":
		u.render(w, http.StatusOK, "parts", "url-form", view{Notice: "Webhook URL removed."})
	default:
		u.render(w, http.StatusOK, "parts", "url-form", view{WebhookURL: got, WebhookAt: at, Notice: "Saved."})
	}
}

// handleTestEvent sends the test event and returns the result plus the
// refreshed delivery log (out of band).
func (u *ui) handleTestEvent(w http.ResponseWriter, r *http.Request, p participant) {
	d, err := u.s.sendTest(r.Context(), p.ID)
	var ie inputError
	switch {
	case errors.As(err, &ie):
		u.render(w, http.StatusOK, "parts", "action-result", view{Error: ie.msg})
		return
	case err != nil:
		u.fail(w, err)
		return
	}
	v := view{Notice: "Test event sent: " + deliveryOutcome(d)}
	if d.Error != "" || d.StatusCode < 200 || d.StatusCode > 299 {
		v.Error, v.Notice = "Test event failed: "+deliveryOutcome(d), ""
	}
	if v.Deliveries, err = u.s.deliveries(r.Context(), p.ID, 50); err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "parts", "action-result-oob", v)
}

func deliveryOutcome(d delivery) string {
	if d.Error != "" {
		return d.Error
	}
	return "HTTP " + strconv.Itoa(d.StatusCode) + " in " + strconv.FormatInt(d.DurationMS, 10) + " ms"
}

func (u *ui) handleReplayUI(w http.ResponseWriter, r *http.Request, p participant) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	n, err := strconv.Atoi(r.PostFormValue("last"))
	if err != nil {
		n = 0 // replay rejects it
	}
	queued, err := u.s.replay(r.Context(), p.ID, n)
	var ie inputError
	switch {
	case errors.As(err, &ie):
		u.render(w, http.StatusOK, "parts", "action-result", view{Error: ie.msg})
	case err != nil:
		u.fail(w, err)
	case queued == 0:
		u.render(w, http.StatusOK, "parts", "action-result", view{Notice: "No GitHub deliveries to replay yet."})
	default:
		u.render(w, http.StatusOK, "parts", "action-result",
			view{Notice: "Replaying " + strconv.Itoa(queued) + " deliveries; they show up in the log."})
	}
}

func (u *ui) handleJournal(w http.ResponseWriter, r *http.Request, p participant) {
	js, err := u.s.listJournal(r.Context(), journalFilter{Participant: p.ID}, 200)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "journal", "layout", view{Title: "Journal", Nav: "journal", Who: p, Journal: js})
}

func (u *ui) handleAdmin(w http.ResponseWriter, r *http.Request) {
	rows, err := u.s.participantsOverview(r.Context())
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "admin", "layout", view{Title: "Overview", Nav: "admin", Admin: true, Overview: rows})
}

func (u *ui) handleAdminParticipant(w http.ResponseWriter, r *http.Request) {
	p, ok := u.s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	v := view{Title: p.ID, Nav: "admin", Admin: true, Participant: p}
	var err error
	if v.Namespaces, err = u.namespaceViews(r.Context(), p.ID); err == nil {
		if v.WebhookURL, v.WebhookAt, err = u.s.webhookSetting(r.Context(), p.ID); err == nil {
			if v.Deliveries, err = u.s.deliveries(r.Context(), p.ID, 50); err == nil {
				v.Journal, err = u.s.listJournal(r.Context(), journalFilter{Participant: p.ID}, 200)
			}
		}
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "admin_participant", "layout", v)
}

func (u *ui) handleAdminJournal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := journalFilter{Participant: q.Get("participant"), Type: q.Get("type"), Status: q.Get("status")}
	js, err := u.s.listJournal(r.Context(), f, 500)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "admin_journal", "layout",
		view{Title: "Journal", Nav: "admin-journal", Admin: true, Journal: js, Filter: f, IDs: u.s.cfg.Participants})
}

func (u *ui) handleAdminGitHub(w http.ResponseWriter, r *http.Request) {
	evs, err := u.s.githubEvents(r.Context(), 200)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, http.StatusOK, "admin_github", "layout", view{Title: "GitHub webhook", Nav: "admin-github", Admin: true, Events: evs})
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(tsLayout, s)
	return t, err == nil
}

// fmtTime shows a stored time in UTC with layout, or "" when unset.
func fmtTime(s, layout string) string {
	t, ok := parseTS(s)
	if !ok {
		return s
	}
	return t.Format(layout)
}

func olderThan(now time.Time, s string, d time.Duration) bool {
	t, ok := parseTS(s)
	return ok && now.Sub(t) > d
}

// age is a short relative time: "40s", "12m", "3h", "2d".
func age(now time.Time, s string) string {
	t, ok := parseTS(s)
	if !ok {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

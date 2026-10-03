package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const maxWebhookBody = 1 << 20

// forwardHeaders are the GitHub headers passed on as is (and stored for replay).
var forwardHeaders = []string{
	"Content-Type", "X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID",
	"X-Hub-Signature-256", "X-Hub-Signature", "User-Agent",
}

// runNameRE matches `run-name` in .github/workflows/deploy.yml:
// deploy ${{ inputs.namespace }} ${{ inputs.version }} break=${{ inputs.break }}
var runNameRE = regexp.MustCompile(`^deploy (\S+) (\S+) break=(\S+)$`)

type runName struct{ Namespace, Version, Break string }

func parseRunName(s string) (runName, bool) {
	m := runNameRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return runName{}, false
	}
	return runName{m[1], m[2], m[3]}, true
}

// signSHA256 returns the X-Hub-Signature-256 value for body.
func signSHA256(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func signSHA1(secret string, body []byte) string {
	m := hmac.New(sha1.New, []byte(secret))
	m.Write(body)
	return "sha1=" + hex.EncodeToString(m.Sum(nil))
}

// validSignature checks X-Hub-Signature-256 in constant time.
func validSignature(secret string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// deploymentStatus is the part of a deployment_status payload the relay reads.
type deploymentStatus struct {
	DeploymentStatus struct {
		State          string `json:"state"`
		EnvironmentURL string `json:"environment_url"`
		TargetURL      string `json:"target_url"`
		LogURL         string `json:"log_url"`
	} `json:"deployment_status"`
	Deployment struct {
		Payload json.RawMessage `json:"payload"`
	} `json:"deployment"`
	WorkflowRun *struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		DisplayTitle string `json:"display_title"`
		HTMLURL      string `json:"html_url"`
	} `json:"workflow_run"`
}

func (p *deploymentStatus) runName() string {
	if p.WorkflowRun == nil {
		return ""
	}
	if p.WorkflowRun.DisplayTitle != "" {
		return p.WorkflowRun.DisplayTitle
	}
	return p.WorkflowRun.Name
}

func (p *deploymentStatus) runURL() string {
	if p.WorkflowRun != nil && p.WorkflowRun.HTMLURL != "" {
		return p.WorkflowRun.HTMLURL
	}
	if p.DeploymentStatus.TargetURL != "" {
		return p.DeploymentStatus.TargetURL
	}
	return p.DeploymentStatus.LogURL
}

// namespace finds the target namespace: deployment_status.environment_url,
// then deployment.payload.environment_url, then the run name. GitHub sends
// statuses before the job ends (in_progress) without environment_url, so the
// run name fallback is what routes them.
func (p *deploymentStatus) namespace(domain string) string {
	urls := []string{p.DeploymentStatus.EnvironmentURL}
	var dp struct {
		EnvironmentURL string `json:"environment_url"`
	}
	if json.Unmarshal(p.Deployment.Payload, &dp) == nil {
		urls = append(urls, dp.EnvironmentURL)
	}
	for _, u := range urls {
		if ns := namespaceFromURL(u, domain); ns != "" {
			return ns
		}
	}
	if rn, ok := parseRunName(p.runName()); ok {
		return rn.Namespace
	}
	return ""
}

// namespaceFromURL returns <ns> for https://<ns>.<domain>/..., or "".
func namespaceFromURL(raw, domain string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	ns, ok := strings.CutSuffix(strings.ToLower(u.Hostname()), "."+strings.ToLower(domain))
	if !ok || ns == "" || strings.Contains(ns, ".") {
		return ""
	}
	return ns
}

func (s *server) handleGitHub(w http.ResponseWriter, r *http.Request) {
	event, gd := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.recordEvent(event, gd, "", "", "bad_request", "body over 1 MiB")
			writeError(w, http.StatusRequestEntityTooLarge, "body over 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	if !validSignature(s.cfg.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		detail := "missing X-Hub-Signature-256"
		if r.Header.Get("X-Hub-Signature-256") != "" {
			detail = "signature mismatch"
		}
		s.log.Warn("github webhook: bad signature", "event", event, "delivery", gd, "remote", r.RemoteAddr, "detail", detail)
		s.recordEvent(event, gd, "", "", "bad_signature", detail+" from "+r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	switch event {
	case "ping":
		s.recordEvent(event, gd, "", "", "ping", "")
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	case "deployment_status":
	default:
		s.recordEvent(event, gd, "", "", "ignored", "event not handled")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}

	var p deploymentStatus
	if err := json.Unmarshal(body, &p); err != nil {
		s.recordEvent(event, gd, "", "", "bad_request", "invalid JSON")
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	ns := p.namespace(s.cfg.TargetDomain)
	owner := s.ownerOf(ns)
	if owner == "" {
		s.log.Info("github webhook: not routed", "delivery", gd, "namespace", ns, "state", p.DeploymentStatus.State)
		s.recordEvent(event, gd, ns, "", "unrouted", "no known participant for namespace "+fmt.Sprintf("%q", ns))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "unrouted"})
		return
	}

	switch p.DeploymentStatus.State {
	case "success", "failure", "error":
		if err := s.journalFromDeploy(owner, ns, &p); err != nil {
			s.log.Error("journal: store deploy entry", "delivery", gd, "err", err)
		}
	}

	target, err := s.webhookURL(r.Context(), owner)
	if err != nil {
		s.log.Error("github webhook: read url", "participant", owner, "err", err)
	}
	if target == "" {
		s.recordEvent(event, gd, ns, owner, "no_url", "participant has no webhook URL")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "no_url"})
		return
	}
	headers := map[string]string{}
	for _, h := range forwardHeaders {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	s.recordEvent(event, gd, ns, owner, "forwarded", target)
	s.async(func() { s.deliver(context.Background(), owner, ns, "github", event, gd, headers, body, target) })
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "forwarded"})
}

// journalFromDeploy writes a deploy entry for a final deployment state.
func (s *server) journalFromDeploy(owner, ns string, p *deploymentStatus) error {
	name := p.runName()
	var version, message string
	if rn, ok := parseRunName(name); ok {
		version = rn.Version
		if rn.Break != "none" {
			message = "break=" + rn.Break
		}
	}
	t := ts(s.now())
	_, err := s.db.Exec(`INSERT INTO journal
		(participant_id, namespace, type, status, version, run_url, run_name, message, created_at, updated_at)
		VALUES (?, ?, 'deploy', ?, ?, ?, ?, ?, ?, ?)`,
		owner, ns, p.DeploymentStatus.State, nullStr(version), nullStr(p.runURL()), nullStr(name), nullStr(message), t, t)
	return err
}

// delivery is one stored forward attempt (without headers and body).
type delivery struct {
	ID             int64  `json:"id"`
	Participant    string `json:"participant"`
	Namespace      string `json:"namespace,omitempty"`
	Kind           string `json:"kind"`
	Event          string `json:"event,omitempty"`
	GitHubDelivery string `json:"github_delivery,omitempty"`
	URL            string `json:"url"`
	StatusCode     int    `json:"status_code,omitempty"`
	Error          string `json:"error,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	CreatedAt      string `json:"created_at"`
}

// deliver sends one attempt and stores it. The URL is checked again here: a
// stored URL may predate the current rules.
func (s *server) deliver(ctx context.Context, pid, ns, kind, event, gd string, headers map[string]string, body []byte, target string) delivery {
	start := s.now()
	d := delivery{Participant: pid, Namespace: ns, Kind: kind, Event: event, GitHubDelivery: gd, URL: target, CreatedAt: ts(start)}
	var err error
	if err = validWebhookURL(target, s.cfg.AllowPrivateTargets); err == nil {
		d.StatusCode, err = send(ctx, s.out, target, headers, body)
	}
	d.DurationMS = s.now().Sub(start).Milliseconds()
	if err != nil {
		d.Error = err.Error()
	}
	hj, _ := json.Marshal(headers)
	res, dbErr := s.db.Exec(`INSERT INTO deliveries
		(participant_id, namespace, kind, event, github_delivery, headers, body, url, status_code, error, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pid, nullStr(ns), kind, nullStr(event), nullStr(gd), string(hj), body, target,
		nullInt(d.StatusCode), nullStr(d.Error), d.DurationMS, d.CreatedAt)
	if dbErr != nil {
		s.log.Error("store delivery", "participant", pid, "err", dbErr)
	} else {
		d.ID, _ = res.LastInsertId()
	}
	s.log.Info("delivery", "participant", pid, "kind", kind, "event", event, "delivery", gd,
		"status", d.StatusCode, "error", d.Error, "duration_ms", d.DurationMS)
	return d
}

// handleTest sends a signed ping-style event to the caller's URL and returns the result.
func (s *server) handleTest(w http.ResponseWriter, r *http.Request, p participant) {
	target, err := s.webhookURL(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "set a webhook URL first")
		return
	}
	body, _ := json.Marshal(map[string]any{
		"zen":         "Lab relay test event.",
		"hook_id":     0,
		"relay_test":  true,
		"participant": p.ID,
		"sent_at":     ts(s.now()),
	})
	gd := newUUID()
	headers := map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   gd,
		"X-GitHub-Hook-ID":    "0",
		"X-Hub-Signature-256": signSHA256(s.cfg.WebhookSecret, body),
		"X-Hub-Signature":     signSHA1(s.cfg.WebhookSecret, body),
		"User-Agent":          "GitHub-Hookshot/lab-relay-test",
	}
	writeJSON(w, http.StatusOK, s.deliver(r.Context(), p.ID, "", "test", "ping", gd, headers, body, target))
}

// handleReplay resends the last N GitHub deliveries of the caller (oldest
// first) with their original headers and body, in the background.
func (s *server) handleReplay(w http.ResponseWriter, r *http.Request, p participant) {
	n := 1
	if v := r.URL.Query().Get("last"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 20 {
			writeError(w, http.StatusBadRequest, "last must be 1..20")
			return
		}
	}
	target, err := s.webhookURL(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "set a webhook URL first")
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT COALESCE(namespace, ''), COALESCE(event, ''),
		COALESCE(github_delivery, ''), headers, body FROM deliveries
		WHERE participant_id = ? AND kind = 'github' ORDER BY id DESC LIMIT ?`, p.ID, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	type orig struct {
		ns, event, gd string
		headers       map[string]string
		body          []byte
	}
	var list []orig
	for rows.Next() {
		var o orig
		var hj string
		if err := rows.Scan(&o.ns, &o.event, &o.gd, &hj, &o.body); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		_ = json.Unmarshal([]byte(hj), &o.headers)
		list = append(list, o)
	}
	rows.Close()
	s.async(func() {
		for i := len(list) - 1; i >= 0; i-- {
			o := list[i]
			s.deliver(context.Background(), p.ID, o.ns, "replay", o.event, o.gd, o.headers, o.body, target)
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": len(list)})
}

// recordEvent stores one received GitHub request for the admin view and
// keeps the newest 1000.
func (s *server) recordEvent(event, gd, ns, pid, outcome, detail string) {
	if _, err := s.db.Exec(`INSERT INTO github_events
		(event, github_delivery, namespace, participant_id, outcome, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullStr(event), nullStr(gd), nullStr(ns), nullStr(pid), outcome, nullStr(detail), ts(s.now())); err != nil {
		s.log.Error("store github event", "err", err)
		return
	}
	_, _ = s.db.Exec(`DELETE FROM github_events WHERE id <= (SELECT MAX(id) FROM github_events) - 1000`)
}

func (s *server) webhookURL(ctx context.Context, pid string) (string, error) {
	var u string
	err := s.db.QueryRowContext(ctx, `SELECT url FROM webhook_urls WHERE participant_id = ?`, pid).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return u, err
}

// async runs f in the background; wait() blocks until all such work is done.
func (s *server) async(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

func (s *server) wait() { s.wg.Wait() }

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

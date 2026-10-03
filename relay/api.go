package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// server holds the HTTP handlers for the webhook and the API.
type server struct {
	cfg  *config
	db   *sql.DB
	log  *slog.Logger
	out  *http.Client // forwards, test events, replays
	byID map[string]participant
	now  func() time.Time
	wg   sync.WaitGroup
}

func newServer(cfg *config, db *sql.DB, log *slog.Logger) *server {
	s := &server{
		cfg:  cfg,
		db:   db,
		log:  log,
		out:  newOutboundClient(10*time.Second, cfg.AllowPrivateTargets),
		byID: map[string]participant{},
		now:  time.Now,
	}
	for _, p := range cfg.Participants {
		s.byID[p.ID] = p
	}
	return s
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhook/github", s.handleGitHub)

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/namespaces", s.participantOnly(s.handleNamespaces))
	mux.HandleFunc("GET /api/webhook", s.participantOnly(s.handleWebhookGet))
	mux.HandleFunc("PUT /api/webhook-url", s.participantOnly(s.handleWebhookPut))
	mux.HandleFunc("POST /api/webhook/test", s.participantOnly(s.handleTest))
	mux.HandleFunc("POST /api/webhook/replay", s.participantOnly(s.handleReplay))
	mux.HandleFunc("GET /api/deliveries", s.participantOnly(s.handleDeliveries))
	mux.HandleFunc("GET /api/journal", s.handleJournalList)

	mux.HandleFunc("GET /api/participants", s.adminOnly(s.handleParticipants))
	mux.HandleFunc("POST /api/journal", s.adminOnly(s.handleJournalCreate))
	mux.HandleFunc("PATCH /api/journal/{id}", s.adminOnly(s.handleJournalPatch))
}

// caller resolves X-Api-Key. Every key is compared in constant time, and the
// loop does not stop at a match.
func (s *server) caller(r *http.Request) (p participant, admin, ok bool) {
	key := []byte(r.Header.Get("X-Api-Key"))
	if len(key) == 0 {
		return participant{}, false, false
	}
	if subtle.ConstantTimeCompare(key, []byte(s.cfg.AdminKey)) == 1 {
		admin, ok = true, true
	}
	for _, c := range s.cfg.Participants {
		if subtle.ConstantTimeCompare(key, []byte(c.APIKey)) == 1 {
			p, ok = c, true
		}
	}
	return p, admin, ok
}

// participantOnly: no or unknown key → 401, admin key → 403.
func (s *server) participantOnly(h func(http.ResponseWriter, *http.Request, participant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, admin, ok := s.caller(r)
		switch {
		case !ok:
			writeError(w, http.StatusUnauthorized, "missing or unknown X-Api-Key")
		case admin:
			writeError(w, http.StatusForbidden, "participant key required")
		default:
			h(w, r, p)
		}
	}
}

// adminOnly: no or unknown key → 401, participant key → 403.
func (s *server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, admin, ok := s.caller(r)
		switch {
		case !ok:
			writeError(w, http.StatusUnauthorized, "missing or unknown X-Api-Key")
		case !admin:
			writeError(w, http.StatusForbidden, "admin key required")
		default:
			h(w, r)
		}
	}
}

// ownerOf returns the participant id from a namespace prefix (`p01-demo` → p01)
// when it is a known participant, connected namespace or not.
func (s *server) ownerOf(ns string) string {
	prefix, _, ok := strings.Cut(ns, "-")
	if _, known := s.byID[prefix]; !ok || !known {
		return ""
	}
	return prefix
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, admin, ok := s.caller(r)
	switch {
	case !ok:
		writeError(w, http.StatusUnauthorized, "missing or unknown X-Api-Key")
	case admin:
		writeJSON(w, http.StatusOK, map[string]any{"admin": true})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name, "admin": false})
	}
}

type namespaceRow struct {
	Name           string `json:"name"`
	Participant    string `json:"participant"`
	Connected      bool   `json:"connected"`
	FirstSeen      string `json:"first_seen"`
	LastSeen       string `json:"last_seen"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
}

func (s *server) handleNamespaces(w http.ResponseWriter, r *http.Request, p participant) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT name, participant_id, connected, first_seen, last_seen,
		COALESCE(disconnected_at, '') FROM namespaces WHERE participant_id = ? ORDER BY name`, p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []namespaceRow{}
	for rows.Next() {
		var n namespaceRow
		if err := rows.Scan(&n.Name, &n.Participant, &n.Connected, &n.FirstSeen, &n.LastSeen, &n.DisconnectedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, n)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleWebhookGet(w http.ResponseWriter, r *http.Request, p participant) {
	var u, updated string
	err := s.db.QueryRowContext(r.Context(), `SELECT url, updated_at FROM webhook_urls WHERE participant_id = ?`, p.ID).Scan(&u, &updated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u, "updated_at": updated, "secret": s.cfg.WebhookSecret})
}

// handleWebhookPut sets the URL from {"url": "..."}; an empty url removes it.
func (s *server) handleWebhookPut(w http.ResponseWriter, r *http.Request, p participant) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, `want JSON {"url": "https://..."}`)
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM webhook_urls WHERE participant_id = ?`, p.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": ""})
		return
	}
	if len(in.URL) > 2048 {
		writeError(w, http.StatusBadRequest, "URL too long")
		return
	}
	if err := validWebhookURL(in.URL, s.cfg.AllowPrivateTargets); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t := ts(s.now())
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO webhook_urls (participant_id, url, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(participant_id) DO UPDATE SET url = excluded.url, updated_at = excluded.updated_at`, p.ID, in.URL, t); err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": in.URL, "updated_at": t})
}

func (s *server) handleDeliveries(w http.ResponseWriter, r *http.Request, p participant) {
	out, err := s.deliveries(r, p.ID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deliveries returns the newest deliveries of a participant, newest first.
func (s *server) deliveries(r *http.Request, pid string, limit int) ([]delivery, error) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, participant_id, COALESCE(namespace, ''), kind,
		COALESCE(event, ''), COALESCE(github_delivery, ''), url, COALESCE(status_code, 0), COALESCE(error, ''),
		COALESCE(duration_ms, 0), created_at FROM deliveries WHERE participant_id = ? ORDER BY id DESC LIMIT ?`, pid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []delivery{}
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.ID, &d.Participant, &d.Namespace, &d.Kind, &d.Event, &d.GitHubDelivery, &d.URL,
			&d.StatusCode, &d.Error, &d.DurationMS, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type journalEntry struct {
	ID          int64  `json:"id"`
	Participant string `json:"participant"`
	Namespace   string `json:"namespace"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Version     string `json:"version,omitempty"`
	RunURL      string `json:"run_url,omitempty"`
	RunName     string `json:"run_name,omitempty"`
	Message     string `json:"message,omitempty"`
	Command     string `json:"command,omitempty"`
	Undo        string `json:"undo,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

const journalCols = `id, participant_id, namespace, type, status, COALESCE(version, ''), COALESCE(run_url, ''),
	COALESCE(run_name, ''), COALESCE(message, ''), COALESCE(command, ''), COALESCE(undo, ''), created_at, updated_at`

func scanJournal(sc interface{ Scan(...any) error }) (journalEntry, error) {
	var e journalEntry
	err := sc.Scan(&e.ID, &e.Participant, &e.Namespace, &e.Type, &e.Status, &e.Version, &e.RunURL,
		&e.RunName, &e.Message, &e.Command, &e.Undo, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// handleJournalList: a participant sees their own entries; the admin sees all
// or one participant's. since (RFC 3339) matches updated_at, so a poller also
// sees entries patched since its last read. Newest first, at most 500.
func (s *server) handleJournalList(w http.ResponseWriter, r *http.Request) {
	p, admin, ok := s.caller(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or unknown X-Api-Key")
		return
	}
	q := r.URL.Query()
	pid := p.ID
	if admin {
		pid = q.Get("participant")
	} else if v := q.Get("participant"); v != "" && v != p.ID {
		writeError(w, http.StatusForbidden, "participant filter is admin only")
		return
	}
	where, args := []string{"1=1"}, []any{}
	if pid != "" {
		where, args = append(where, "participant_id = ?"), append(args, pid)
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC 3339")
			return
		}
		where, args = append(where, "updated_at >= ?"), append(args, ts(t))
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+journalCols+` FROM journal WHERE `+
		strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []journalEntry{}
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleJournalCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Namespace string `json:"namespace"`
		Type      string `json:"type"`
		Message   string `json:"message"`
		Command   string `json:"command"`
		Undo      string `json:"undo"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Status == "" {
		in.Status = "open"
	}
	owner := s.ownerOf(in.Namespace)
	switch {
	case owner == "":
		writeError(w, http.StatusBadRequest, "namespace must start with a known participant id")
		return
	case in.Type != "deploy" && in.Type != "drift":
		writeError(w, http.StatusBadRequest, "type must be deploy or drift")
		return
	case in.Status != "open" && in.Status != "fixed":
		writeError(w, http.StatusBadRequest, "status must be open or fixed")
		return
	}
	t := ts(s.now())
	res, err := s.db.ExecContext(r.Context(), `INSERT INTO journal
		(participant_id, namespace, type, status, message, command, undo, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, owner, in.Namespace, in.Type, in.Status,
		nullStr(in.Message), nullStr(in.Command), nullStr(in.Undo), t, t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	id, _ := res.LastInsertId()
	s.writeJournalEntry(w, r, http.StatusCreated, id)
}

func (s *server) handleJournalPatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such entry")
		return
	}
	var in struct {
		Status  *string `json:"status"`
		Message *string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Status == nil && in.Message == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: status or message")
		return
	}
	if in.Status != nil && *in.Status != "open" && *in.Status != "fixed" {
		writeError(w, http.StatusBadRequest, "status must be open or fixed")
		return
	}
	set, args := []string{"updated_at = ?"}, []any{ts(s.now())}
	if in.Status != nil {
		set, args = append(set, "status = ?"), append(args, *in.Status)
	}
	if in.Message != nil {
		set, args = append(set, "message = ?"), append(args, nullStr(*in.Message))
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE journal SET `+strings.Join(set, ", ")+` WHERE id = ?`, append(args, id)...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "no such entry")
		return
	}
	s.writeJournalEntry(w, r, http.StatusOK, id)
}

func (s *server) writeJournalEntry(w http.ResponseWriter, r *http.Request, status int, id int64) {
	e, err := scanJournal(s.db.QueryRowContext(r.Context(), `SELECT `+journalCols+` FROM journal WHERE id = ?`, id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, status, e)
}

type participantRow struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Namespaces    []string  `json:"namespaces"`
	WebhookURL    string    `json:"webhook_url"`
	LastDelivery  *delivery `json:"last_delivery"`
	LastJournalAt string    `json:"last_journal_at"`
}

func (s *server) handleParticipants(w http.ResponseWriter, r *http.Request) {
	out := make([]participantRow, 0, len(s.cfg.Participants))
	for _, p := range s.cfg.Participants {
		row := participantRow{ID: p.ID, Name: p.Name, Namespaces: []string{}}
		err := func() error {
			rows, err := s.db.QueryContext(r.Context(), `SELECT name FROM namespaces
				WHERE participant_id = ? AND connected = 1 ORDER BY name`, p.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					rows.Close()
					return err
				}
				row.Namespaces = append(row.Namespaces, n)
			}
			rows.Close()
			if row.WebhookURL, err = s.webhookURL(r.Context(), p.ID); err != nil {
				return err
			}
			ds, err := s.deliveries(r, p.ID, 1)
			if err != nil {
				return err
			}
			if len(ds) == 1 {
				row.LastDelivery = &ds[0]
			}
			return s.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(created_at), '') FROM journal
				WHERE participant_id = ?`, p.ID).Scan(&row.LastJournalAt)
		}()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

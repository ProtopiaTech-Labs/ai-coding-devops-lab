package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// migrations run in order; PRAGMA user_version stores how many have run.
// Append new entries, never edit applied ones.
var migrations = []string{
	`CREATE TABLE namespaces (
		name            TEXT PRIMARY KEY,
		participant_id  TEXT NOT NULL,
		connected       INTEGER NOT NULL,
		first_seen      TEXT NOT NULL,
		last_seen       TEXT NOT NULL,
		disconnected_at TEXT
	);
	CREATE INDEX namespaces_participant ON namespaces(participant_id);

	CREATE TABLE webhook_urls (
		participant_id TEXT PRIMARY KEY,
		url            TEXT NOT NULL,
		updated_at     TEXT NOT NULL
	);

	-- One row per forward attempt: a GitHub event, a test event or a replay.
	CREATE TABLE deliveries (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		participant_id  TEXT NOT NULL,
		namespace       TEXT,
		kind            TEXT NOT NULL,  -- github, test, replay
		github_delivery TEXT,           -- X-GitHub-Delivery
		headers         TEXT NOT NULL,  -- JSON object of forwarded headers
		body            BLOB NOT NULL,
		url             TEXT NOT NULL,
		status_code     INTEGER,
		error           TEXT,
		duration_ms     INTEGER,
		created_at      TEXT NOT NULL
	);
	CREATE INDEX deliveries_participant ON deliveries(participant_id, id);

	CREATE TABLE journal (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		participant_id TEXT NOT NULL,
		namespace      TEXT NOT NULL,
		type           TEXT NOT NULL,  -- deploy, agent, manual
		status         TEXT NOT NULL,  -- deploy: the deployment state; agent/manual: open, fixed
		version        TEXT,
		run_url        TEXT,
		run_name       TEXT,
		message        TEXT,
		created_at     TEXT NOT NULL,
		updated_at     TEXT NOT NULL
	);
	CREATE INDEX journal_participant ON journal(participant_id, created_at);
	CREATE INDEX journal_created ON journal(created_at);`,

	// Webhook and API (T04, T05).
	`ALTER TABLE deliveries ADD COLUMN event TEXT;  -- X-GitHub-Event
	ALTER TABLE journal ADD COLUMN command TEXT;    -- drift: what the agent ran
	ALTER TABLE journal ADD COLUMN undo TEXT;       -- drift: how to revert it
	CREATE INDEX journal_updated ON journal(updated_at);

	-- Every POST /webhook/github, for the admin view: routed, unrouted, ignored, HMAC errors.
	CREATE TABLE github_events (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		event           TEXT,
		github_delivery TEXT,
		namespace       TEXT,
		participant_id  TEXT,
		outcome         TEXT NOT NULL,  -- forwarded, no_url, unrouted, ignored, ping, bad_signature, bad_request
		detail          TEXT,
		created_at      TEXT NOT NULL
	);`,

	// Journal types by source: deploy, agent, manual (was drift).
	`UPDATE journal SET type = 'manual' WHERE type = 'drift';`,

	// Unused: lists filter by participant or updated_at.
	`DROP INDEX journal_created;`,
}

// openDB opens (or creates) the SQLite file and applies pending migrations.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One writer is all a single-replica relay needs, and it rules out SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies the migrations newer than PRAGMA user_version. Running it
// again is a no-op.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

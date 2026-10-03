package main

import (
	"path/filepath"
	"testing"
)

func TestMigrateTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO namespaces VALUES ('p01-demo', 'p01', 1, 'a', 'a', NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	db.Close()

	// Reopening runs migrate again on an existing file and keeps the data.
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v, n int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Errorf("user_version = %d, %v; want %d", v, err, len(migrations))
	}
	if err := db.QueryRow(`SELECT count(*) FROM namespaces`).Scan(&n); err != nil || n != 1 {
		t.Errorf("namespaces rows = %d, %v; want 1", n, err)
	}
	for _, table := range []string{"webhook_urls", "deliveries", "journal"} {
		if _, err := db.Exec(`SELECT * FROM ` + table + ` LIMIT 0`); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

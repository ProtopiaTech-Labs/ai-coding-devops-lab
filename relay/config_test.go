package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseParticipants(t *testing.T) {
	got, err := parseParticipants(strings.NewReader(`
# comment
p01,Jan Kowalski: 3f2a0000-0000-4000-8000-000000000001

  # indented comment
p02 , Anna: Nowak : key-2
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []participant{
		{"p01", "Jan Kowalski", "3f2a0000-0000-4000-8000-000000000001"},
		{"p02", "Anna: Nowak", "key-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseParticipantsErrors(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"duplicate id", "p01,A: k1\np01,B: k2", "line 2: duplicate id p01"},
		{"duplicate key", "p01,A: k1\np02,B: k1", "line 2: duplicate api key"},
		{"bad id", "p1,A: k1", `id "p1"`},
		{"bad id prefix", "x01,A: k1", `id "x01"`},
		{"no comma", "p01 A: k1", "line 1: want"},
		{"no colon", "p01,A k1", "line 1: want"},
		{"empty name", "p01,: k1", "empty display name"},
		{"empty key", "p01,A:", "empty api key"},
		{"key with space", "p01,A: k 1", "api key with spaces"},
		{"only comments", "# none\n\n", "no participants"},
	}
	for _, tt := range tests {
		_, err := parseParticipants(strings.NewReader(tt.in))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("PARTICIPANTS_FILE", write("participants.txt", "p01,Jan: k1\n"))
	t.Setenv("ADMIN_KEY_FILE", write("admin-key", "admin\n"))
	t.Setenv("WEBHOOK_SECRET_FILE", write("webhook-secret", "s3cret\n"))
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminKey != "admin" || c.WebhookSecret != "s3cret" || c.DiscoveryInterval != 30*time.Second ||
		c.DBPath != "/data/relay.db" || c.TargetDomain != "lab.patoarchitekci.io" || len(c.Participants) != 1 {
		t.Errorf("config = %+v", c)
	}

	t.Setenv("ADMIN_KEY_FILE", write("admin-key", "k1"))
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "admin key") {
		t.Errorf("admin key = participant key: err %v", err)
	}
	t.Setenv("ADMIN_KEY_FILE", write("admin-key", " \n"))
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty admin key: err %v", err)
	}
}

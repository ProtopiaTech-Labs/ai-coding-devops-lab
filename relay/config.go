package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// participant is one line of participants.txt.
type participant struct {
	ID     string
	Name   string
	APIKey string
}

type config struct {
	Participants      []participant
	AdminKey          string
	WebhookSecret     string
	DBPath            string
	Port              string
	TargetDomain      string
	DiscoveryInterval time.Duration
	// AllowPrivateTargets lets webhook URLs use http:// and loopback or
	// private addresses. Local tests only, never in the cluster.
	AllowPrivateTargets bool
}

var participantID = regexp.MustCompile(`^p[0-9]{2}$`)

// loadConfig reads the environment and the files it points to.
func loadConfig() (*config, error) {
	c := &config{
		DBPath:       envOr("DB_PATH", "/data/relay.db"),
		Port:         envOr("PORT", "8080"),
		TargetDomain: envOr("TARGET_DOMAIN", "lab.patoarchitekci.io"),

		AllowPrivateTargets: os.Getenv("RELAY_ALLOW_PRIVATE_TARGETS") == "true",
	}
	var err error
	if c.DiscoveryInterval, err = time.ParseDuration(envOr("DISCOVERY_INTERVAL", "30s")); err != nil || c.DiscoveryInterval <= 0 {
		return nil, fmt.Errorf("invalid DISCOVERY_INTERVAL %q", os.Getenv("DISCOVERY_INTERVAL"))
	}

	path := envOr("PARTICIPANTS_FILE", "/etc/relay/participants.txt")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if c.Participants, err = parseParticipants(f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if c.AdminKey, err = readSecret("ADMIN_KEY_FILE", "/etc/relay/secret/admin-key"); err != nil {
		return nil, err
	}
	if c.WebhookSecret, err = readSecret("WEBHOOK_SECRET_FILE", "/etc/relay/secret/webhook-secret"); err != nil {
		return nil, err
	}
	for _, p := range c.Participants {
		if p.APIKey == c.AdminKey {
			return nil, fmt.Errorf("participant %s uses the admin key", p.ID)
		}
	}
	return c, nil
}

// readSecret reads a non-empty, trimmed value from the file named by env (or def).
func readSecret(env, def string) (string, error) {
	path := envOr(env, def)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", env, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: %s is empty", env, path)
	}
	return s, nil
}

// parseParticipants reads lines `<id>,<display name>: <api key>`. Blank lines
// and lines starting with # are skipped. Any malformed line, duplicate id or
// duplicate key is an error, so the relay never starts with an ambiguous list.
func parseParticipants(r io.Reader) ([]participant, error) {
	var out []participant
	ids, keys := map[string]int{}, map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, rest, ok := strings.Cut(line, ",")
		i := strings.LastIndex(rest, ":")
		if !ok || i < 0 {
			return nil, fmt.Errorf("line %d: want `<id>,<display name>: <api key>`", n)
		}
		p := participant{
			ID:     strings.TrimSpace(id),
			Name:   strings.TrimSpace(rest[:i]),
			APIKey: strings.TrimSpace(rest[i+1:]),
		}
		switch {
		case !participantID.MatchString(p.ID):
			return nil, fmt.Errorf("line %d: id %q does not match p00..p99", n, p.ID)
		case p.Name == "":
			return nil, fmt.Errorf("line %d: empty display name", n)
		case p.APIKey == "" || strings.ContainsAny(p.APIKey, " \t"):
			return nil, fmt.Errorf("line %d: empty api key or api key with spaces", n)
		}
		if prev, dup := ids[p.ID]; dup {
			return nil, fmt.Errorf("line %d: duplicate id %s (first on line %d)", n, p.ID, prev)
		}
		if prev, dup := keys[p.APIKey]; dup {
			return nil, fmt.Errorf("line %d: duplicate api key (first on line %d)", n, prev)
		}
		ids[p.ID], keys[p.APIKey] = n, n
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no participants")
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

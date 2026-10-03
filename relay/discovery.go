package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	saDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
	targetLabel = "lab.protopia.tech/target=true"
)

// kubeClient reads the Kubernetes API: the target namespaces, and for the
// namespace screen the shop Deployment and the pods.
type kubeClient struct {
	base   string // API server URL without a trailing slash
	client *http.Client
	token  func() (string, error)
}

// newKubeClient uses KUBE_API (and optional KUBE_TOKEN) when set, e.g.
// KUBE_API=http://127.0.0.1:8001 with `kubectl proxy`; otherwise the
// in-cluster ServiceAccount.
func newKubeClient() (*kubeClient, error) {
	if api := os.Getenv("KUBE_API"); api != "" {
		token := os.Getenv("KUBE_TOKEN")
		return &kubeClient{
			base:   strings.TrimSuffix(api, "/"),
			client: &http.Client{Timeout: 10 * time.Second},
			token:  func() (string, error) { return token, nil },
		}, nil
	}

	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set and no KUBE_API")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s/ca.crt", saDir)
	}
	return &kubeClient{
		base: "https://" + net.JoinHostPort(host, port),
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
		// Read the token on every call: projected tokens are rotated by the kubelet.
		token: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

// namespaces returns the names of the labelled namespaces.
func (k *kubeClient) namespaces(ctx context.Context) ([]string, error) {
	body, err := k.get(ctx, "/api/v1/namespaces?labelSelector="+url.QueryEscape(targetLabel))
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	return parseNamespaceList(body)
}

// get GETs path on the API server and returns the body of a 200 response.
func (k *kubeClient) get(ctx context.Context, path string) ([]byte, error) {
	token, err := k.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiStatusError{resp.StatusCode, fmt.Sprintf("%.200s", body)}
	}
	return body, nil
}

type apiStatusError struct {
	code int
	body string
}

func (e *apiStatusError) Error() string { return fmt.Sprintf("status %d: %s", e.code, e.body) }

// parseNamespaceList returns the names in a Kubernetes NamespaceList.
func parseNamespaceList(body []byte) ([]string, error) {
	var list struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	if list.Kind != "NamespaceList" {
		return nil, fmt.Errorf("unexpected kind %q", list.Kind)
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Metadata.Name)
	}
	return out, nil
}

// discovery keeps the namespaces table in line with the cluster.
type discovery struct {
	db      *sql.DB
	log     *slog.Logger
	known   map[string]bool // participant ids
	ignored map[string]bool // names already logged as ignored
}

func newDiscovery(db *sql.DB, log *slog.Logger, ps []participant) *discovery {
	d := &discovery{db: db, log: log, known: map[string]bool{}, ignored: map[string]bool{}}
	for _, p := range ps {
		d.known[p.ID] = true
	}
	return d
}

// owner returns the participant id from the name prefix (`p01-demo` → p01),
// or "" when the prefix is not a known participant.
func (d *discovery) owner(name string) string {
	prefix, _, ok := strings.Cut(name, "-")
	if !ok || !d.known[prefix] {
		return ""
	}
	return prefix
}

// reconcile applies one namespace list: new names are inserted connected,
// disconnected names that are listed again reconnect, and connected names
// that are missing (label removed or namespace deleted) disconnect.
// Names without a known owner are ignored and, if stored, disconnect.
func (d *discovery) reconcile(ctx context.Context, names []string, now time.Time) error {
	listed := map[string]string{} // name → owner
	for _, n := range names {
		o := d.owner(n)
		if o == "" {
			if !d.ignored[n] {
				d.ignored[n] = true
				d.log.Warn("namespace ignored: no known participant prefix", "namespace", n)
			}
			continue
		}
		listed[n] = o
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type row struct {
		owner     string
		connected bool
	}
	stored := map[string]row{}
	rows, err := tx.QueryContext(ctx, `SELECT name, participant_id, connected FROM namespaces`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		var r row
		if err := rows.Scan(&n, &r.owner, &r.connected); err != nil {
			rows.Close()
			return err
		}
		stored[n] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	t := ts(now)
	type change struct{ name, owner, state, reason string }
	var changes []change
	for n, o := range listed {
		r, exists := stored[n]
		switch {
		case !exists:
			_, err = tx.ExecContext(ctx, `INSERT INTO namespaces
				(name, participant_id, connected, first_seen, last_seen) VALUES (?, ?, 1, ?, ?)`, n, o, t, t)
			changes = append(changes, change{n, o, "connected", "new"})
		case !r.connected:
			_, err = tx.ExecContext(ctx, `UPDATE namespaces
				SET connected = 1, last_seen = ?, disconnected_at = NULL WHERE name = ?`, t, n)
			changes = append(changes, change{n, o, "connected", "reappeared"})
		default:
			_, err = tx.ExecContext(ctx, `UPDATE namespaces SET last_seen = ? WHERE name = ?`, t, n)
		}
		if err != nil {
			return err
		}
	}
	for n, r := range stored {
		if _, ok := listed[n]; ok || !r.connected {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE namespaces
			SET connected = 0, disconnected_at = ? WHERE name = ?`, t, n); err != nil {
			return err
		}
		changes = append(changes, change{n, r.owner, "disconnected", "not listed"})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slices.SortFunc(changes, func(a, b change) int { return strings.Compare(a.name, b.name) })
	for _, c := range changes {
		d.log.Info("namespace "+c.state, "namespace", c.name, "participant", c.owner, "state", c.state, "reason", c.reason)
	}
	return nil
}

// run lists and reconciles once now and then every interval until ctx ends.
// A failed list keeps the stored state: an API outage must not disconnect everyone.
func (d *discovery) run(ctx context.Context, k *kubeClient, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		names, err := k.namespaces(ctx)
		if err != nil {
			d.log.Error("discovery failed, keeping previous state", "err", err)
		} else if err := d.reconcile(ctx, names, time.Now()); err != nil {
			d.log.Error("discovery: store failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// shopDeployment is the Deployment whose image tag is the shop version shown
// on the namespace screen.
const shopDeployment = "orders"

// workload is what the namespace screen shows from the cluster.
type workload struct {
	Version string // image tag of the shop Deployment; "" when it does not exist
	Ready   int    // pods with condition Ready
	Total   int    // pods that are not Succeeded or Failed
	Problem string // first waiting or terminated reason of a pod that is not ready
	Err     string // the read failed
}

// workload reads the shop Deployment's image tag and the pod readiness of ns.
func (k *kubeClient) workload(ctx context.Context, ns string) workload {
	var w workload
	esc := url.PathEscape(ns)
	body, err := k.get(ctx, "/apis/apps/v1/namespaces/"+esc+"/deployments/"+shopDeployment)
	var se *apiStatusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
	case err != nil:
		w.Err = "read deployment: " + err.Error()
		return w
	default:
		w.Version = parseDeploymentVersion(body)
	}
	if body, err = k.get(ctx, "/api/v1/namespaces/"+esc+"/pods"); err != nil {
		w.Err = "list pods: " + err.Error()
		return w
	}
	w.Ready, w.Total, w.Problem = parsePodList(body)
	return w
}

// parseDeploymentVersion returns the tag of the first container image.
func parseDeploymentVersion(body []byte) string {
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &d) != nil || len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	img := d.Spec.Template.Spec.Containers[0].Image
	img, _, _ = strings.Cut(img, "@")
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		return img[i+1:]
	}
	return "latest"
}

// parsePodList counts ready pods among the running ones and returns the first
// reason a pod is not ready (CrashLoopBackOff, ImagePullBackOff, Pending, ...).
func parsePodList(body []byte) (ready, total int, problem string) {
	var list struct {
		Items []struct {
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					State map[string]struct {
						Reason string `json:"reason"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return 0, 0, "unreadable pod list"
	}
	for _, p := range list.Items {
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		total++
		isReady := false
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				isReady = true
			}
		}
		if isReady {
			ready++
			continue
		}
		if problem != "" {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			for _, st := range []string{"waiting", "terminated"} {
				if r := cs.State[st].Reason; r != "" && problem == "" {
					problem = r
				}
			}
		}
		if problem == "" {
			problem = p.Status.Phase
		}
	}
	return ready, total, problem
}

// workloadCache keeps workload reads for ttl, so page loads and polls do not
// hit the API server every time.
type workloadCache struct {
	read func(ctx context.Context, ns string) workload
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	m    map[string]cachedWorkload
}

type cachedWorkload struct {
	at time.Time
	w  workload
}

func newWorkloadCache(read func(context.Context, string) workload, ttl time.Duration) *workloadCache {
	return &workloadCache{read: read, ttl: ttl, now: time.Now, m: map[string]cachedWorkload{}}
}

func (c *workloadCache) get(ctx context.Context, ns string) workload {
	c.mu.Lock()
	e, ok := c.m[ns]
	c.mu.Unlock()
	if ok && c.now().Sub(e.at) < c.ttl {
		return e.w
	}
	w := c.read(ctx, ns)
	c.mu.Lock()
	c.m[ns] = cachedWorkload{c.now(), w}
	c.mu.Unlock()
	return w
}

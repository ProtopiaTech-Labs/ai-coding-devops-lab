package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	saDir          = "/var/run/secrets/kubernetes.io/serviceaccount"
	targetLabel    = "lab.protopia.tech/target=true"
	discoveryEvery = 30 * time.Second
)

// runLoadgen sends POST /orders to every target each interval. It serves
// /healthz and /metrics only: no chaos endpoints, and no /readyz because
// nothing depends on loadgen being ready.
func runLoadgen(log *slog.Logger, port string) {
	interval, err := time.ParseDuration(envOr("LOADGEN_INTERVAL", "1s"))
	if err != nil || interval <= 0 {
		log.Error("invalid LOADGEN_INTERVAL", "value", os.Getenv("LOADGEN_INTERVAL"))
		os.Exit(2)
	}
	client := &http.Client{Timeout: 5 * time.Second}

	// Explicit TARGETS win; otherwise discover namespaces in the cluster.
	discover := func() ([]string, error) { return parseTargets(os.Getenv("TARGETS")), nil }
	if os.Getenv("TARGETS") == "" {
		k, err := newKubeClient()
		if err != nil {
			log.Error("no TARGETS and no in-cluster config", "err", err)
			os.Exit(2)
		}
		domain := envOr("TARGET_DOMAIN", "lab.patoarchitekci.io")
		discover = func() ([]string, error) { return k.targets(domain) }
	}

	m := newMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", m.handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	go func() {
		srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()
	log.Info("starting", "role", "loadgen", "version", version, "port", port, "interval", interval.String())

	var targets atomic.Pointer[[]string]
	refresh := func() {
		t, err := discover()
		if err != nil {
			log.Error("discovery failed, keeping previous targets", "err", err)
			return
		}
		if old := targets.Load(); old == nil || strings.Join(t, ",") != strings.Join(*old, ",") {
			log.Info("targets", "targets", t)
		}
		targets.Store(&t)
		m.setTargets(t)
	}
	refresh()
	// Discovery runs on its own goroutine so a slow API call never delays a send tick.
	go func() {
		for range time.Tick(discoveryEvery) {
			refresh()
		}
	}()
	for range time.Tick(interval) {
		for _, t := range *targets.Load() {
			go sendOrder(log, client, m, t)
		}
	}
}

// sendOrder sends one POST /orders, records it in m and logs the result.
func sendOrder(log *slog.Logger, client *http.Client, m *metrics, target string) {
	start := time.Now()
	status, versions, errMsg := 0, map[string]string(nil), ""
	resp, err := client.Post(target+"/orders", "application/json", nil)
	if err != nil {
		errMsg = err.Error()
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		status = resp.StatusCode
		versions, errMsg = parseOrderResponse(body)
	}
	elapsed := time.Since(start)
	m.observe(target, status, elapsed)
	log.Info("order", "target", target, "status", status,
		"latency_ms", elapsed.Milliseconds(), "versions", versions, "error", errMsg)
}

// parseOrderResponse returns the versions of a 201 body, or the error of a
// failed one. A body that is not JSON (e.g. an Ingress error page) is an error.
func parseOrderResponse(body []byte) (map[string]string, string) {
	var r struct {
		Versions map[string]string `json:"versions"`
		Error    string            `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, "invalid response: " + err.Error()
	}
	return r.Versions, r.Error
}

// parseTargets splits a comma-separated list of base URLs.
func parseTargets(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSuffix(strings.TrimSpace(t), "/"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseNamespaceList turns a Kubernetes NamespaceList into target base URLs.
func parseNamespaceList(body []byte, domain string) ([]string, error) {
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
	out := []string{}
	for _, it := range list.Items {
		out = append(out, "https://"+it.Metadata.Name+"."+domain)
	}
	return out, nil
}

// kubeClient lists namespaces with the in-cluster ServiceAccount.
type kubeClient struct {
	url    string
	client *http.Client
}

func newKubeClient() (*kubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set")
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
		url: "https://" + net.JoinHostPort(host, port) +
			"/api/v1/namespaces?labelSelector=" + url.QueryEscape(targetLabel),
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}, nil
}

func (k *kubeClient) targets(domain string) ([]string, error) {
	// Read the token on every call: projected tokens are rotated by the kubelet.
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
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
		return nil, fmt.Errorf("list namespaces: status %d: %.200s", resp.StatusCode, body)
	}
	return parseNamespaceList(body, domain)
}

// Command relay discovers participant namespaces in the lab cluster, forwards
// GitHub deployment_status webhooks to their owners and keeps a chaos journal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at build time: -ldflags "-X main.version=1.0.N".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(2)
	}
	db, err := openDB(cfg.DBPath)
	if err != nil {
		log.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	kube, err := newKubeClient()
	if err != nil {
		log.Error("no Kubernetes API", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	api := newServer(cfg, db, log)
	api.routes(mux)
	web, err := newUI(api, newWorkloadCache(kube.workload, 15*time.Second).get)
	if err != nil {
		log.Error("ui templates", "err", err)
		os.Exit(1)
	}
	web.routes(mux)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	log.Info("starting", "version", version, "port", cfg.Port, "participants", len(cfg.Participants),
		"db", cfg.DBPath, "discovery_interval", cfg.DiscoveryInterval.String(), "health_metrics_url", cfg.HealthMetricsURL)
	if cfg.AllowPrivateTargets {
		log.Warn("RELAY_ALLOW_PRIVATE_TARGETS=true: webhook URLs may use http:// and private addresses (local tests only)")
	}

	go newDiscovery(db, log, cfg.Participants).run(ctx, kube, cfg.DiscoveryInterval)
	go api.health.run(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
	api.wait() // in-flight forwards end within their 10 s timeout
	log.Info("stopped")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Command shop runs one of the lab shop services, selected by ROLE.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// version is set at build time: -ldflags "-X main.version=1.0.N".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	role := os.Getenv("ROLE")
	port := envOr("PORT", "8080")
	if role == "loadgen" {
		runLoadgen(log, port)
		return
	}

	ch := newChaos()
	mux := http.NewServeMux()
	registerCommon(mux, role, ch)

	switch role {
	case "orders":
		o := &orders{
			inventoryURL: os.Getenv("INVENTORY_URL"),
			paymentsURL:  os.Getenv("PAYMENTS_URL"),
			client:       &http.Client{Timeout: 2 * time.Second},
		}
		mux.HandleFunc("POST /orders", o.create)
	case "inventory":
		mux.HandleFunc("POST /reserve", versionHandler(role))
	case "payments":
		mux.HandleFunc("POST /charge", versionHandler(role))
	default:
		log.Error("unknown ROLE", "role", role)
		os.Exit(2)
	}

	if m := os.Getenv("CHAOS"); m != "" {
		mode, err := parseMode(m)
		if err != nil {
			log.Error("invalid CHAOS", "chaos", m)
			os.Exit(2)
		}
		log.Warn("chaos at start", "mode", mode)
		ch.apply(mode)
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           logRequests(log, role, ch.slowdown(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	log.Info("starting", "role", role, "version", version, "port", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// registerCommon adds the endpoints every service role has.
func registerCommon(mux *http.ServeMux, role string, ch *chaos) {
	mux.HandleFunc("GET /version", versionHandler(role))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ch.unready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /chaos/{mode}", ch.handle)
}

func versionHandler(role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": role, "version": version})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests writes one JSON log line per request.
func logRequests(log *slog.Logger, role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// 499 (nginx convention): the client went away before the response.
		status, canceled := rec.status, r.Context().Err() != nil
		if canceled {
			status = 499
		}
		log.Info("request", "role", role, "method", r.Method, "path", r.URL.Path,
			"status", status, "canceled", canceled, "duration_ms", time.Since(start).Milliseconds())
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

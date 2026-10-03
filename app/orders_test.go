package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func upstream(status int, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		writeJSON(w, status, map[string]string{"version": "1.0.7"})
	}))
}

func TestCreateOrder(t *testing.T) {
	ok := upstream(http.StatusOK, 0)
	defer ok.Close()
	down := upstream(http.StatusOK, 0)
	down.Close() // connection refused
	bad := upstream(http.StatusInternalServerError, 0)
	defer bad.Close()
	slow := upstream(http.StatusOK, 300*time.Millisecond)
	defer slow.Close()

	tests := []struct {
		name               string
		inventory, payment string
		want               int
	}{
		{"both ok", ok.URL, ok.URL, http.StatusCreated},
		{"payments down", ok.URL, down.URL, http.StatusBadGateway},
		{"inventory 500", bad.URL, ok.URL, http.StatusBadGateway},
		{"inventory timeout", slow.URL, ok.URL, http.StatusGatewayTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &orders{
				inventoryURL: tt.inventory,
				paymentsURL:  tt.payment,
				client:       &http.Client{Timeout: 100 * time.Millisecond},
			}
			rec := httptest.NewRecorder()
			o.create(rec, httptest.NewRequest(http.MethodPost, "/orders", nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want != http.StatusCreated {
				return
			}
			var body struct{ Versions map[string]string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"orders", "inventory", "payments"} {
				if body.Versions[s] == "" {
					t.Errorf("missing version of %s: %v", s, body.Versions)
				}
			}
		})
	}
}

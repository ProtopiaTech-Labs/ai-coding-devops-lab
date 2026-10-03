package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type orders struct {
	inventoryURL string
	paymentsURL  string
	client       *http.Client
}

// upstreamError carries the HTTP status orders returns for a failed call.
type upstreamError struct {
	service string
	status  int // 502 or 504
	err     error
}

func (e *upstreamError) Error() string { return fmt.Sprintf("%s: %v", e.service, e.err) }

func (o *orders) create(w http.ResponseWriter, r *http.Request) {
	inv, err := o.call(r.Context(), "inventory", o.inventoryURL+"/reserve")
	if err == nil {
		var pay string
		pay, err = o.call(r.Context(), "payments", o.paymentsURL+"/charge")
		if err == nil {
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": newID(),
				"versions": map[string]string{
					"orders": version, "inventory": inv, "payments": pay,
				},
			})
			return
		}
	}
	var ue *upstreamError
	errors.As(err, &ue)
	writeJSON(w, ue.status, map[string]string{"error": ue.Error(), "upstream": ue.service})
}

// call POSTs to an upstream service and returns its version.
func (o *orders) call(ctx context.Context, service, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", &upstreamError{service, http.StatusBadGateway, err}
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", &upstreamError{service, statusFor(err), err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", &upstreamError{service, http.StatusBadGateway, fmt.Errorf("status %d", resp.StatusCode)}
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", &upstreamError{service, statusFor(err), err}
	}
	return body.Version, nil
}

// statusFor maps a transport error to 504 for timeouts and 502 otherwise.
func statusFor(err error) int {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

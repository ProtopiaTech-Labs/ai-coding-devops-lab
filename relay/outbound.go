package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// errBlockedTarget is returned when a webhook URL resolves to an address the
// relay must not call (it runs inside the cluster, next to the API server and
// the cloud metadata endpoint).
var errBlockedTarget = errors.New("target address not allowed")

// blockedAddr reports whether ip is loopback, private (RFC 1918, fc00::/7),
// link-local (169.254.0.0/16, fe80::/10), unspecified or multicast.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// newOutboundClient returns the client for forwards, test events and replays:
// one attempt, the given timeout, no redirects, and unless allowPrivate a
// check of the address actually dialed (after DNS), so a rebinding answer
// cannot swap a public IP for an internal one.
func newOutboundClient(timeout time.Duration, allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", errBlockedTarget, address)
			}
			if blockedAddr(ap.Addr()) {
				return fmt.Errorf("%w: %s", errBlockedTarget, ap.Addr())
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // never route participant URLs through an env proxy
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: timeout,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// validWebhookURL checks a participant URL: https with a host; http too when
// allowHTTP (local tests only).
func validWebhookURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
	default:
		return fmt.Errorf("URL must start with https://")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("URL has no host")
	}
	if u.User != nil {
		return fmt.Errorf("URL must not contain credentials")
	}
	return nil
}

// send POSTs body with headers to target and returns the status code.
func send(ctx context.Context, c *http.Client, target string, headers map[string]string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

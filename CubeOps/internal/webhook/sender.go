// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

const (
	defaultUserAgent = "cubeops-webhook"
	maxResponseBody  = 1 << 20 // 1 MiB
)

// Sender posts delivery payloads to subscription endpoints with HMAC signing
// and SSRF-protected dialing. One shared Sender (its own Transport) serves
// all workers, so webhook traffic never shares the management API
// connection pool.
type Sender struct {
	client *http.Client
}

// NewSender builds a Sender with a dedicated Transport: single-resolution DNS
// pinning via a custom DialContext, no redirects, and an overall request
// timeout of httpTimeout.
func NewSender(httpTimeout time.Duration, allowPrivateNetworks bool) *Sender {
	transport := &http.Transport{
		DialContext:           pinnedDialContext(allowPrivateNetworks),
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: httpTimeout,
	}
	return &Sender{
		client: &http.Client{
			Transport:     transport,
			Timeout:       httpTimeout,
			CheckRedirect: rejectRedirect,
		},
	}
}

// newSenderWithClient is the test seam: it wires a Sender over a caller
// supplied *http.Client (e.g. a fake RoundTripper) so HTTP logic can be
// unit-tested without binding a listener. Production uses NewSender.
func newSenderWithClient(c *http.Client) *Sender {
	return &Sender{client: c}
}

// SendResult classifies one delivery attempt.
type SendResult struct {
	Class      string // ResultSucceeded | ResultRetryable | ResultPermanent | ResultShutdown
	HTTPStatus int
	Err        error
}

// Send performs one HTTP POST. The context is the supervisor's; if graceful
// shutdown cancels it before the request completes, the result is classified
// ResultShutdown (no attempt recorded). An HTTP timeout or transport error
// with a live context is ResultRetryable.
func (s *Sender) Send(ctx context.Context, d *DeliveryForSend) SendResult {
	body := bytes.NewReader(d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, body)
	if err != nil {
		return SendResult{Class: ResultPermanent, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("X-Cube-Event-ID", d.EventID)
	req.Header.Set("X-Cube-Delivery", fmt.Sprintf("%s:%d", d.EventID, d.SubscriptionID))
	req.Header.Set("X-Cube-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if d.Secret != "" {
		mac := hmac.New(sha256.New, []byte(d.Secret))
		_, _ = mac.Write(d.Payload)
		req.Header.Set("X-Cube-Signature-256", hex.EncodeToString(mac.Sum(nil)))
	}

	start := time.Now()
	resp, err := s.client.Do(req)
	deliveryDuration.WithLabelValues("total").Observe(time.Since(start).Seconds())
	if err != nil {
		// A canceled context is the graceful-shutdown / outer-cancel signal:
		// the send was interrupted locally, not a genuine endpoint failure.
		if ctx.Err() == context.Canceled {
			deliveryResultTotal.WithLabelValues(ResultShutdown).Inc()
			return SendResult{Class: ResultShutdown, Err: err}
		}
		deliveryResultTotal.WithLabelValues(ResultRetryable).Inc()
		return SendResult{Class: ResultRetryable, Err: err}
	}
	defer resp.Body.Close()
	// Bound response body reads; anything beyond the cap is discarded.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	httpStatusTotal.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		deliveryResultTotal.WithLabelValues(ResultSucceeded).Inc()
		return SendResult{Class: ResultSucceeded, HTTPStatus: resp.StatusCode}
	case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500:
		deliveryResultTotal.WithLabelValues(ResultRetryable).Inc()
		return SendResult{Class: ResultRetryable, HTTPStatus: resp.StatusCode}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Redirects are treated as network-style failures per the spec.
		deliveryResultTotal.WithLabelValues(ResultRetryable).Inc()
		return SendResult{Class: ResultRetryable, HTTPStatus: resp.StatusCode}
	default:
		deliveryResultTotal.WithLabelValues(ResultPermanent).Inc()
		return SendResult{Class: ResultPermanent, HTTPStatus: resp.StatusCode}
	}
}

// rejectRedirect refuses to follow any HTTP redirect (301/302/303/307/308).
// http.ErrUseLastResponse makes the client return the 3xx response itself,
// which the classifier maps to a retryable network-style failure.
func rejectRedirect(*http.Request, []*http.Request) error {
	redirectRejectedTotal.Inc()
	return http.ErrUseLastResponse
}

// alwaysDeniedCIDRs are rejected regardless of allow_private_networks
// (documentation / CGNAT / multicast ranges that can never be a legitimate
// endpoint).
var alwaysDeniedCIDRs = mustPrefixes(
	"0.0.0.0/8",     // "this network"
	"100.64.0.0/10", // CGNAT
	"192.0.0.0/24",  // IETF protocol assignments
	"198.18.0.0/15", // benchmarking
	"224.0.0.0/4",   // multicast
	"::/128",        // unspecified
	"ff00::/8",      // multicast
)

// privateCIDRs are denied unless allow_private_networks=true. This covers
// loopback, link-local (incl. the cloud metadata address) and RFC1918 — the
// local smoke receiver listens on 127.0.0.1, so the dev flag must relax all
// of these. The flag is documented as dev/test-only.
var privateCIDRs = mustPrefixes(
	"127.0.0.0/8",    // loopback
	"::1/128",        // loopback
	"169.254.0.0/16", // link-local + metadata (169.254.169.254)
	"fe80::/10",      // link-local
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// pinnedDialContext resolves the host once, validates every address, then
// dials the first valid IP directly. The http.Transport keeps the original
// URL hostname for TLS ServerName and the Host header, so IP pinning neither
// bypasses nor breaks certificate validation.
func pinnedDialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var dialIP netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			if err := checkSSRFAddr(ip, allowPrivate); err != nil {
				ssrfRejectedTotal.Inc()
				return nil, err
			}
			dialIP = ip
		} else {
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no addresses resolved for %s", host)
			}
			for _, ip := range ips {
				if err := checkSSRFAddr(ip, allowPrivate); err != nil {
					ssrfRejectedTotal.Inc()
					return nil, fmt.Errorf("ssrf policy rejected %s for %s: %w", ip, host, err)
				}
			}
			dialIP = ips[0]
		}
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
	}
}

// checkSSRFAddr applies the unified CIDR policy. IPv4-mapped IPv6 addresses
// are unmapped first so ::ffff:127.0.0.1 is rejected as loopback.
func checkSSRFAddr(ip netip.Addr, allowPrivate bool) error {
	ip = ip.Unmap()
	for _, p := range alwaysDeniedCIDRs {
		if p.Contains(ip) {
			return fmt.Errorf("address %s is in denied range %s", ip, p)
		}
	}
	if !allowPrivate {
		for _, p := range privateCIDRs {
			if p.Contains(ip) {
				return fmt.Errorf("address %s is private (allow_private_networks=false)", ip)
			}
		}
	}
	return nil
}

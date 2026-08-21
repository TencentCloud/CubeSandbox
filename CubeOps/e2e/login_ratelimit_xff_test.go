//go:build e2e

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// badLogin sends one failing login attempt, optionally asserting a client IP via
// X-Forwarded-For, and returns the HTTP status.
func badLogin(t *testing.T, baseURL, forwardedFor string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"definitely-not-the-password"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func countThrottled(t *testing.T, baseURL string, attempts int, forwardedFor func(i int) string) int {
	t.Helper()
	throttled := 0
	for i := 0; i < attempts; i++ {
		xff := ""
		if forwardedFor != nil {
			xff = forwardedFor(i)
		}
		if badLogin(t, baseURL, xff) == http.StatusTooManyRequests {
			throttled++
		}
	}
	return throttled
}

// TestRotatingForwardedForCannotBypassTheLimiterEndToEnd is the wire-level guard
// for #1377: with no trusted proxies configured the limiter must key on the real
// TCP peer, so rotating the header cannot buy a fresh quota.
func TestRotatingForwardedForCannotBypassTheLimiterEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	inst := e.start(t)
	defer inst.stop()

	throttled := countThrottled(t, inst.baseURL, 40, func(i int) string {
		return fmt.Sprintf("203.0.113.%d", i%250+1)
	})
	if throttled == 0 {
		t.Fatal("rotating X-Forwarded-For bypassed the login rate limiter entirely")
	}
	t.Logf("rotating header: %d/40 throttled", throttled)
}

// TestWhitespacePaddedForwardedForCannotBypassTheLimiterEndToEnd covers the
// padding variant, which the old hand-rolled parser also treated as distinct.
func TestWhitespacePaddedForwardedForCannotBypassTheLimiterEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	inst := e.start(t)
	defer inst.stop()

	throttled := countThrottled(t, inst.baseURL, 40, func(i int) string {
		return strings.Repeat(" ", i%8) + "203.0.113.9"
	})
	if throttled == 0 {
		t.Fatal("whitespace-padded X-Forwarded-For bypassed the login rate limiter")
	}
	t.Logf("padded header: %d/40 throttled", throttled)
}

// TestTrustedProxyForwardedForIsHonouredEndToEnd is the counterpart: once the
// operator declares the proxy, distinct clients behind it get distinct buckets.
// A fix that simply ignored the header would fail this.
func TestTrustedProxyForwardedForIsHonouredEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	inst := e.start(t, "CUBE_OPS_TRUSTED_PROXIES=127.0.0.1,::1")
	defer inst.stop()

	noisy := countThrottled(t, inst.baseURL, 40, func(int) string { return "198.51.100.7" })
	if noisy == 0 {
		t.Fatal("a client behind a trusted proxy was never throttled")
	}

	quiet := countThrottled(t, inst.baseURL, 3, func(int) string { return "198.51.100.8" })
	if quiet != 0 {
		t.Fatalf("a different client behind the same trusted proxy was throttled %d/3 times", quiet)
	}
	t.Logf("trusted proxy: noisy %d/40 throttled, quiet %d/3 throttled", noisy, quiet)
}

// TestTrustAllProxiesIsRejectedAtStartupEndToEnd proves the config guard aborts
// the process rather than silently restoring the pre-fix behaviour.
func TestTrustAllProxiesIsRejectedAtStartupEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	warm := e.start(t)
	warm.stop()

	out := e.startExpectingExit(t, "CUBE_OPS_TRUSTED_PROXIES=0.0.0.0/0")
	if !strings.Contains(out, "trusts every source address") {
		t.Fatalf("0.0.0.0/0 was not rejected with the expected message:\n%s", tailLines(out, 15))
	}
}

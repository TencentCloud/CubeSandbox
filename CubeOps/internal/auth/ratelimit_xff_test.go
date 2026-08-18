// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newLimiterRouter(t *testing.T, trustedProxies []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.POST("/login", LoginRateLimit(), func(c *gin.Context) {
		markLoginFailure(c)
		c.Status(http.StatusUnauthorized)
	})
	return r
}

func attempt(r *gin.Engine, peer string, xff string) int {
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = peer
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func countBlocked(r *gin.Engine, peer string, xffs []string) int {
	blocked := 0
	for _, xff := range xffs {
		if attempt(r, peer, xff) == http.StatusTooManyRequests {
			blocked++
		}
	}
	return blocked
}

func resetLimiter(t *testing.T) {
	t.Helper()
	defaultLoginLimiter.mu.Lock()
	defaultLoginLimiter.failures = map[string][]time.Time{}
	defaultLoginLimiter.mu.Unlock()
}

func TestRotatingForwardedForCannotBypassTheLimiter(t *testing.T) {
	resetLimiter(t)
	r := newLimiterRouter(t, nil)

	xffs := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		xffs = append(xffs, "10.1.1."+string(rune('0'+i%10))+"9")
	}

	blocked := countBlocked(r, "203.0.113.9:1234", xffs)
	if blocked == 0 {
		t.Fatal("rotating X-Forwarded-For bypassed the limiter entirely")
	}
	if blocked < 30 {
		t.Fatalf("only %d/40 attempts were blocked; the peer IP is not being used as the key", blocked)
	}
}

func TestWhitespacePaddedForwardedForCannotBypassTheLimiter(t *testing.T) {
	resetLimiter(t)
	r := newLimiterRouter(t, nil)

	xffs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		pad := ""
		for j := 0; j < i; j++ {
			pad += " "
		}
		xffs = append(xffs, pad+"198.51.100.7")
	}

	if blocked := countBlocked(r, "203.0.113.9:1234", xffs); blocked == 0 {
		t.Fatal("whitespace-padded X-Forwarded-For bypassed the limiter")
	}
}

func TestLimiterStillFiresWithoutForwardedFor(t *testing.T) {
	resetLimiter(t)
	r := newLimiterRouter(t, nil)

	blocked := 0
	for i := 0; i < 10; i++ {
		if attempt(r, "203.0.113.9:1234", "") == http.StatusTooManyRequests {
			blocked++
		}
	}
	if blocked == 0 {
		t.Fatal("baseline broken: the limiter never fired")
	}
}

func TestDistinctPeerIPsGetIndependentBuckets(t *testing.T) {
	resetLimiter(t)
	r := newLimiterRouter(t, nil)

	for i := 0; i < 6; i++ {
		attempt(r, "203.0.113.9:1234", "")
	}
	if code := attempt(r, "203.0.113.9:1234", ""); code != http.StatusTooManyRequests {
		t.Fatalf("first peer should be blocked, got %d", code)
	}
	if code := attempt(r, "203.0.113.10:1234", ""); code == http.StatusTooManyRequests {
		t.Fatal("a different peer IP was blocked by another peer's failures")
	}
}

func TestTrustedProxyForwardedForIsHonoured(t *testing.T) {
	resetLimiter(t)
	r := newLimiterRouter(t, []string{"203.0.113.9"})

	for i := 0; i < 6; i++ {
		attempt(r, "203.0.113.9:1234", "198.51.100.7")
	}
	if code := attempt(r, "203.0.113.9:1234", "198.51.100.7"); code != http.StatusTooManyRequests {
		t.Fatalf("a client behind a trusted proxy should be blocked, got %d", code)
	}
	if code := attempt(r, "203.0.113.9:1234", "198.51.100.8"); code == http.StatusTooManyRequests {
		t.Fatal("a different client behind the same trusted proxy was blocked")
	}
}

func TestSweepBoundsTheFailureMap(t *testing.T) {
	l := &loginLimiter{failures: map[string][]time.Time{}, limit: 5, window: time.Millisecond}
	for i := 0; i < sweepThreshold+50; i++ {
		l.recordFailure("10.9." + string(rune('0'+i%10)) + "." + string(rune('0'+(i/10)%10)))
	}
	time.Sleep(5 * time.Millisecond)
	l.recordFailure("10.9.9.9")

	l.mu.Lock()
	size := len(l.failures)
	l.mu.Unlock()
	if size > sweepThreshold {
		t.Fatalf("failure map grew to %d entries, above the sweep threshold %d", size, sweepThreshold)
	}
}

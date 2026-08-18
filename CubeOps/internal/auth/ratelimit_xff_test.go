// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
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

func distinctIP(i int) string {
	return fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
}

func mapSize(l *loginLimiter) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.failures)
}

func TestExpiredEntriesAreSweptOnceTheThresholdIsReached(t *testing.T) {
	l := &loginLimiter{failures: map[string][]time.Time{}, limit: 5, window: 20 * time.Millisecond}
	for i := 0; i < sweepThreshold; i++ {
		l.recordFailure(distinctIP(i))
	}
	if got := mapSize(l); got != sweepThreshold {
		t.Fatalf("map holds %d entries before the sweep, want %d", got, sweepThreshold)
	}

	time.Sleep(40 * time.Millisecond)
	l.recordFailure(distinctIP(sweepThreshold))

	if got := mapSize(l); got != 1 {
		t.Fatalf("map holds %d entries after the sweep, want only the fresh one", got)
	}
}

func TestLiveEntriesAreEvictedSoTheMapStaysBounded(t *testing.T) {
	l := &loginLimiter{failures: map[string][]time.Time{}, limit: 5, window: time.Hour}
	const total = sweepThreshold * 3
	low := total
	for i := 0; i < total; i++ {
		l.recordFailure(distinctIP(i))
		got := mapSize(l)
		if got > sweepThreshold {
			t.Fatalf("map grew to %d entries at i=%d, above the threshold %d", got, i, sweepThreshold)
		}
		if i >= total-sweepThreshold && got < low {
			low = got
		}
	}
	if low > evictTarget+1 {
		t.Fatalf("map never fell below %d entries, so live entries are never evicted", low)
	}
}

func TestEvictionKeepsTheMostRecentAttackers(t *testing.T) {
	l := &loginLimiter{failures: map[string][]time.Time{}, limit: 5, window: time.Hour}
	for i := 0; i < sweepThreshold-6; i++ {
		l.recordFailure(distinctIP(i))
	}

	recent := "203.0.113.7"
	for i := 0; i < 5; i++ {
		l.recordFailure(recent)
	}
	l.recordFailure(distinctIP(sweepThreshold))

	if !l.isBlocked(recent) {
		t.Fatal("the most recently active IP was evicted, so its failures were forgotten")
	}
}

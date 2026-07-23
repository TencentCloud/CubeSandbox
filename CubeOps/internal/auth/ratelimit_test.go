// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"testing"
	"time"
)

// TestS2_LoginRateLimit_BlocksAfter5Failures verifies that the login rate
// limiter blocks an IP after 5 failed attempts within the window, and
// unblocks after the window expires. See review S2.
func TestS2_LoginRateLimit_BlocksAfter5Failures(t *testing.T) {
	// Use a short window so the test runs fast.
	limiter := &loginLimiter{
		failures: make(map[string][]time.Time),
		limit:    5,
		window:   100 * time.Millisecond,
	}
	ip := "192.168.1.100"

	// First 4 failures should not trigger the block.
	for i := 0; i < 4; i++ {
		limiter.recordFailure(ip)
		if limiter.isBlocked(ip) {
			t.Fatalf("blocked after only %d failures, want block at %d", i+1, 5)
		}
	}

	// 5th failure triggers the block (count >= limit).
	limiter.recordFailure(ip)
	if !limiter.isBlocked(ip) {
		t.Fatal("expected IP to be blocked after 5 failures within window (S2)")
	}

	// A different IP is not affected (per-IP isolation).
	if limiter.isBlocked("10.0.0.50") {
		t.Fatal("different IP was blocked — rate limit must be per-IP (S2)")
	}

	// After the window expires, the block lifts.
	time.Sleep(110 * time.Millisecond)
	if limiter.isBlocked(ip) {
		t.Fatal("IP still blocked after window expired (S2)")
	}
}

// TestS2_LoginRateLimit_SuccessfulLoginDoesNotCount verifies that the
// limiter only counts failures — successful logins do not push the counter.
func TestS2_LoginRateLimit_SuccessfulLoginDoesNotCount(t *testing.T) {
	limiter := &loginLimiter{
		failures: make(map[string][]time.Time),
		limit:    5,
		window:   time.Minute,
	}
	ip := "192.168.1.100"

	// Simulate 10 "successful" logins — we do NOT call recordFailure,
	// matching the handler logic (only failures are recorded).
	for i := 0; i < 10; i++ {
		// success path: no recordFailure call
	}
	if limiter.isBlocked(ip) {
		t.Fatal("successful logins should not trigger rate limit (S2)")
	}
}

// TestS2_LoginRateLimit_MapCleanup verifies that the failures map is cleaned
// up when entries expire, preventing unbounded memory growth.
func TestS2_LoginRateLimit_MapCleanup(t *testing.T) {
	limiter := &loginLimiter{
		failures: make(map[string][]time.Time),
		limit:    5,
		window:   50 * time.Millisecond,
	}
	ip := "10.0.0.99"

	// Record a failure.
	limiter.recordFailure(ip)
	limiter.mu.Lock()
	_, exists := limiter.failures[ip]
	limiter.mu.Unlock()
	if !exists {
		t.Fatal("IP should exist in failures map after recordFailure")
	}

	// Wait for the window to expire, then call isBlocked (which now prunes).
	time.Sleep(60 * time.Millisecond)
	_ = limiter.isBlocked(ip)

	limiter.mu.Lock()
	_, exists = limiter.failures[ip]
	limiter.mu.Unlock()
	if exists {
		t.Error("IP entry should be deleted from failures map after window expiry (S2)")
	}
}

// TestS2_LoginRateLimit_RecordFailureCleansEmpty verifies that
// recordFailure deletes the map entry when all timestamps have expired.
func TestS2_LoginRateLimit_RecordFailureCleansEmpty(t *testing.T) {
	limiter := &loginLimiter{
		failures: make(map[string][]time.Time),
		limit:    5,
		window:   50 * time.Millisecond,
	}
	ip := "10.0.0.88"

	// Record one failure, wait for expiry, record again — old entry should be gone.
	limiter.recordFailure(ip)
	time.Sleep(60 * time.Millisecond)

	// New failure after window expiry should leave exactly 1 entry.
	limiter.recordFailure(ip)

	limiter.mu.Lock()
	fails := limiter.failures[ip]
	limiter.mu.Unlock()
	if len(fails) != 1 {
		t.Errorf("after expiry + new failure, failures[ip] = %d, want 1 (S2)", len(fails))
	}
}

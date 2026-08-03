// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"fmt"
	"testing"
	"time"
)

func TestTerminalTicketLimiterBoundsUsersAndPrunesCurrentUserWindow(t *testing.T) {
	limiter := newTerminalTicketLimiter(2, time.Minute)
	now := time.Unix(1_000, 0)

	if !limiter.allowAt("sam", now) || !limiter.allowAt("sam", now.Add(time.Second)) {
		t.Fatal("first two ticket requests should be allowed")
	}
	if limiter.allowAt("sam", now.Add(2*time.Second)) {
		t.Fatal("third ticket request in the window should be rejected")
	}
	if !limiter.allowAt("other", now.Add(2*time.Second)) {
		t.Fatal("one user's rate limit must not block another user")
	}
	if !limiter.allowAt("sam", now.Add(2*time.Minute)) {
		t.Fatal("ticket request should be allowed after the window expires")
	}
	if got := len(limiter.attempts["sam"]); got != 1 {
		t.Fatalf("current user retained %d attempts after window pruning, want 1", got)
	}
	if got := len(limiter.attempts["other"]); got != 1 {
		t.Fatalf("unrelated user attempts changed on sam's hot path: got %d, want 1", got)
	}
	if !limiter.allowAt("other", now.Add(2*time.Minute)) {
		t.Fatal("other user should be allowed after its window expires")
	}
	if got := len(limiter.attempts["other"]); got != 1 {
		t.Fatalf("other user retained %d attempts after its own window pruning, want 1", got)
	}
}

func TestTerminalSessionLimiterEnforcesAndReleasesLimits(t *testing.T) {
	t.Run("per sandbox", func(t *testing.T) {
		limiter := newTerminalSessionLimiter(10, 10, 1)
		release, ok := limiter.acquire("sam", "sandbox-a")
		if !ok {
			t.Fatal("first session should be allowed")
		}
		if _, ok := limiter.acquire("other", "sandbox-a"); ok {
			t.Fatal("sandbox session limit should be enforced")
		}
		release()
		release()
		if _, ok := limiter.acquire("other", "sandbox-a"); !ok {
			t.Fatal("idempotent release should free the sandbox slot")
		}
	})

	t.Run("per user", func(t *testing.T) {
		limiter := newTerminalSessionLimiter(10, 1, 10)
		release, ok := limiter.acquire("sam", "sandbox-a")
		if !ok {
			t.Fatal("first session should be allowed")
		}
		if _, ok := limiter.acquire("sam", "sandbox-b"); ok {
			t.Fatal("user session limit should be enforced")
		}
		release()
	})

	t.Run("global", func(t *testing.T) {
		limiter := newTerminalSessionLimiter(1, 10, 10)
		release, ok := limiter.acquire("sam", "sandbox-a")
		if !ok {
			t.Fatal("first session should be allowed")
		}
		if _, ok := limiter.acquire("other", "sandbox-b"); ok {
			t.Fatal("global session limit should be enforced")
		}
		release()
		if limiter.global != 0 || len(limiter.byUser) != 0 || len(limiter.bySandbox) != 0 {
			t.Fatalf("released limiter retained state: %+v", limiter)
		}
	})
}

func TestTerminalSessionLimiterEnforcesGlobalLimitConcurrently(t *testing.T) {
	const (
		workers   = 32
		maxGlobal = 8
	)
	limiter := newTerminalSessionLimiter(maxGlobal, workers, workers)
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan bool, workers)
	done := make(chan struct{}, workers)

	for i := 0; i < workers; i++ {
		go func(index int) {
			<-start
			releaseSession, ok := limiter.acquire(
				fmt.Sprintf("user-%d", index),
				fmt.Sprintf("sandbox-%d", index),
			)
			results <- ok
			if ok {
				<-release
				releaseSession()
			}
			done <- struct{}{}
		}(i)
	}

	close(start)
	acquired := 0
	for i := 0; i < workers; i++ {
		if <-results {
			acquired++
		}
	}
	if acquired != maxGlobal {
		t.Fatalf("concurrent acquires = %d, want %d", acquired, maxGlobal)
	}

	close(release)
	for i := 0; i < workers; i++ {
		<-done
	}
	if limiter.global != 0 || len(limiter.byUser) != 0 || len(limiter.bySandbox) != 0 {
		t.Fatalf("released limiter retained state: %+v", limiter)
	}
}

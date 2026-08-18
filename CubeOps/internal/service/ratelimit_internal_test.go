// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"sync"
	"testing"
	"time"
)

func TestAllowTestCall_WindowReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc := &WebhookService{
		testLimits: map[int64]*testWindow{},
		now:        func() time.Time { return now },
	}
	for i := 0; i < testWindowMaxCalls; i++ {
		if !svc.allowTestCall(1) {
			t.Fatalf("call %d should be allowed within window", i+1)
		}
	}
	if svc.allowTestCall(1) {
		t.Fatal("6th call must be limited")
	}

	// Advance past the window: counter resets and a new burst is allowed.
	now = now.Add(testWindowDuration + time.Second)
	for i := 0; i < testWindowMaxCalls; i++ {
		if !svc.allowTestCall(1) {
			t.Fatalf("post-reset call %d should be allowed", i+1)
		}
	}
	if svc.allowTestCall(1) {
		t.Fatal("6th call after reset must be limited")
	}

	// A different subscription has its own independent window.
	if !svc.allowTestCall(2) {
		t.Fatal("independent subscription must not be limited")
	}
}

func TestAllowTestCall_ConcurrentSafe(t *testing.T) {
	svc := NewWebhookService(nil)
	const calls = 20
	results := make([]bool, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = svc.allowTestCall(7)
		}(i)
	}
	wg.Wait()
	allowed := 0
	for _, ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != testWindowMaxCalls {
		t.Fatalf("exactly %d concurrent calls must pass, got %d", testWindowMaxCalls, allowed)
	}
}

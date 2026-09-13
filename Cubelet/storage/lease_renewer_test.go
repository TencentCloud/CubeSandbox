// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// TestRenewerRegisterIdempotent guards against double-registration
// (issue #1690/#1692). Pause retries must not double-schedule a renewer.
func TestRenewerRegisterIdempotent(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()

	if err := r.Register(context.Background(), "sb-1", "snap-aaa", cow.BackendS3); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Second Register for the same snapID is a no-op (and returns nil).
	if err := r.Register(context.Background(), "sb-1", "snap-aaa", cow.BackendS3); err != nil {
		t.Fatalf("duplicate Register should be no-op, got: %v", err)
	}
	if got := r.Active(); got != 1 {
		t.Fatalf("Active() = %d, want 1 (duplicate Register must not double-register)", got)
	}
	r.Unregister(context.Background(), "snap-aaa")
}

// TestRenewerUnregisterUnknown confirms unregistering a snap we never
// registered is safe — Destroy paths are allowed to fire even when the
// Pause path never reached the register step.
func TestRenewerUnregisterUnknown(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()

	// Must not panic or block.
	r.Unregister(context.Background(), "snap-never-seen")
}

// TestRenewerSkipsXFS confirms the renewer is a no-op for XFS /
// unknown backends. The pause path calls Register unconditionally;
// if the sandbox was paused onto XFS the renewer must not spin.
func TestRenewerSkipsXFS(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()

	if err := r.Register(context.Background(), "sb-xfs", "snap-xfs", cow.BackendXFS); err != nil {
		t.Fatalf("Register on XFS should be a no-op, got: %v", err)
	}
	if got := r.Active(); got != 0 {
		t.Fatalf("Active() = %d, want 0 (XFS must not register)", got)
	}

	if err := r.Register(context.Background(), "sb-empty", "snap-empty", ""); err != nil {
		t.Fatalf("Register on empty backend should be a no-op, got: %v", err)
	}
	if got := r.Active(); got != 0 {
		t.Fatalf("Active() = %d, want 0 (empty backend must not register)", got)
	}
}

// TestRenewerStops confirms the renewer drains within bounded time
// when Stop is called.
func TestRenewerStops(t *testing.T) {
	r := NewRenewer()
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}

// TestRenewerDoesNotTickOnXFS exercises the timer-resolution regression
// guard: an XFS Register must not produce a goroutine that fires the
// ticker. We can't observe the goroutine directly, but we can assert
// the renewer has zero active entries.
func TestRenewerDoesNotTickOnXFS(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()
	if err := r.Register(context.Background(), "sb", "snap", cow.BackendXFS); err != nil {
		t.Fatal(err)
	}
	if got := r.Active(); got != 0 {
		t.Fatalf("XFS Register leaked an entry: active=%d", got)
	}
}

// TestRenewerConcurrentRegister exercises the lock path with multiple
// goroutines registering distinct snapIDs simultaneously. After the
// dust settles, the renewer must hold exactly one entry per snapID
// and the duplicated Register on the same snapID (separate goroutines)
// must not produce two entries.
//
// The original version used a single snapID across all goroutines and
// asserted a meaningless "Active() ≤ 1" — that passed even when the
// renewer was misbehaving, because every duplicate Register for the
// same key trivially leaves one entry. This version uses N distinct
// snapIDs to actually exercise the per-key uniqueness guarantee.
func TestRenewerConcurrentRegister(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()

	const N = 16
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			snapID := fmt.Sprintf("snap-concurrent-%d", idx)
			if err := r.Register(context.Background(), "sb", snapID, cow.BackendS3); err != nil {
				t.Errorf("Register(%s): %v", snapID, err)
			}
		}(i)
	}
	wg.Wait()

	if got := r.Active(); got != N {
		t.Fatalf("Active() = %d, want %d (one entry per distinct snapID)", got, N)
	}

	// Idempotency: re-registering the same set of keys must not
	// grow the map.
	for i := 0; i < N; i++ {
		snapID := fmt.Sprintf("snap-concurrent-%d", i)
		if err := r.Register(context.Background(), "sb", snapID, cow.BackendS3); err != nil {
			t.Errorf("duplicate Register(%s): %v", snapID, err)
		}
	}
	if got := r.Active(); got != N {
		t.Fatalf("Active() after duplicates = %d, want %d", got, N)
	}
}

// TestRenewerUnregisterAbortsInFlight verifies the abort path added
// after the #1690/#1692 review: closing entry.cancel during a renew
// returns UploadSnapshot promptly (not after the 60 s timeout).
// We can't easily inspect the actual abort in unit tests because
// UploadSnapshot is the production cubecow entry point; the next-best
// check is that Unregister returns within a small bound when the
// renewer is mid-tick.
func TestRenewerUnregisterAbortsInFlight(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()
	if err := r.Register(context.Background(), "sb-abort", "snap-abort", cow.BackendS3); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		r.Unregister(context.Background(), "snap-abort")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Unregister did not return within 2s; renew goroutine is wedged")
	}
}

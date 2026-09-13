// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"sync"
	"sync/atomic"
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
// goroutines registering simultaneously. The map must end up with
// exactly one entry per distinct snapID.
func TestRenewerConcurrentRegister(t *testing.T) {
	r := NewRenewer()
	defer r.Stop()

	const N = 16
	var wg sync.WaitGroup
	var duplicates atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Register(context.Background(), "sb", "snap-concurrent", cow.BackendS3)
			if err != nil {
				t.Errorf("Register: %v", err)
			}
		}()
	}
	// Also fire Unregister from another goroutine to race the lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		r.Unregister(context.Background(), "snap-concurrent")
		duplicates.Add(1)
	}()
	wg.Wait()
	// Allow a moment for any deferred state to settle.
	time.Sleep(50 * time.Millisecond)
	if got := r.Active(); got > 1 {
		t.Fatalf("Active() = %d, want ≤ 1 after concurrent Register/Unregister", got)
	}
}

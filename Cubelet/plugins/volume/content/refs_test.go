// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustHold(t *testing.T, r *MasterRefs, id string) func() {
	t.Helper()
	rel, err := r.Hold(context.Background(), id)
	if err != nil {
		t.Fatalf("hold %q: %v", id, err)
	}
	return rel
}

func TestHoldTakesOneReferenceAndGivesItBack(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	r, _ := newRefs(t, m)

	rel := mustHold(t, r, "vol-1")
	if m.refcount("vol-1") != 1 {
		t.Fatalf("refcount while held = %d, want 1", m.refcount("vol-1"))
	}
	if err := m.deleteVolume("vol-1"); err == nil {
		t.Fatal("a volume that is held must not be deletable")
	}
	rel()
	rel() // releasing twice must not give back a second reference
	if m.refcount("vol-1") != 0 {
		t.Fatalf("refcount after release = %d, want 0", m.refcount("vol-1"))
	}
	if m.appliedRelease.Load() != 1 {
		t.Fatalf("release applied %d times, want 1", m.appliedRelease.Load())
	}
	if err := m.deleteVolume("vol-1"); err != nil {
		t.Fatalf("an idle volume must be deletable: %v", err)
	}
}

// Back-to-back requests do not keep the volume pinned afterwards: a delete
// right after the last request must work.
func TestNothingIsHeldBetweenRequests(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	r, _ := newRefs(t, m)

	for i := 0; i < 3; i++ {
		mustHold(t, r, "vol-1")()
		if m.refcount("vol-1") != 0 {
			t.Fatalf("request %d left refcount at %d", i, m.refcount("vol-1"))
		}
	}
	if m.acquires.Load() != 3 || m.appliedRelease.Load() != 3 {
		t.Fatalf("acquires=%d releases=%d, want 3/3", m.acquires.Load(), m.appliedRelease.Load())
	}
}

func TestConcurrentRequestsShareOneReference(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	r, _ := newRefs(t, m)

	const n = 20
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := r.Hold(context.Background(), "vol-1")
			if err != nil {
				t.Errorf("hold: %v", err)
				return
			}
			releases[i] = rel
		}(i)
	}
	wg.Wait()
	if m.acquires.Load() != 1 || m.refcount("vol-1") != 1 {
		t.Fatalf("acquires=%d refcount=%d, want one shared reference", m.acquires.Load(), m.refcount("vol-1"))
	}

	// The reference outlives all but the last request.
	for i := 0; i < n-1; i++ {
		releases[i]()
	}
	if m.refcount("vol-1") != 1 {
		t.Fatalf("reference released while a request was still running (refcount=%d)", m.refcount("vol-1"))
	}
	releases[n-1]()
	if m.refcount("vol-1") != 0 || m.releases.Load() != 1 {
		t.Fatalf("refcount=%d releases=%d, want 0/1", m.refcount("vol-1"), m.releases.Load())
	}
}

func TestDeletedVolumeCannotBeHeld(t *testing.T) {
	m := newFakeMaster(t)
	r, clk := newRefs(t, m) // the volume was never created or has been deleted

	for i := 0; i < 3; i++ {
		if _, err := r.Hold(context.Background(), "gone"); !errors.Is(err, ErrVolumeNotFound) {
			t.Fatalf("hold %d: %v, want ErrVolumeNotFound", i, err)
		}
	}
	if m.acquires.Load() != 1 {
		t.Fatalf("CubeMaster calls = %d, want 1 (a missing volume is remembered briefly)", m.acquires.Load())
	}

	clk.advance(missingTTL + time.Second)
	m.addVolume("gone") // re-created
	rel, err := r.Hold(context.Background(), "gone")
	if err != nil {
		t.Fatalf("hold after the cache expired: %v", err)
	}
	rel()
}

func TestHoldFailsClosedWhenCubeMasterIsUnreachable(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	m.down.Store(true)
	r, clk := newRefs(t, m)

	_, err := r.Hold(context.Background(), "vol-1")
	if err == nil || errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("unreachable CubeMaster must be an unknown answer, got %v", err)
	}
	// A burst does not retry against the dead server every time.
	for i := 0; i < 5; i++ {
		if _, err := r.Hold(context.Background(), "vol-1"); err == nil {
			t.Fatal("hold succeeded while CubeMaster was unreachable")
		}
	}

	m.down.Store(false)
	if _, err := r.Hold(context.Background(), "vol-1"); err == nil {
		t.Fatal("the failure is remembered briefly, so this must still fail")
	}
	clk.advance(failedTTL + time.Second)
	mustHold(t, r, "vol-1")() // errors are not remembered for long
	if m.acquires.Load() != 1 {
		t.Fatalf("CubeMaster acquires = %d, want 1", m.acquires.Load())
	}
}

func TestUnconfiguredCubeMasterFailsClosed(t *testing.T) {
	r := NewMasterRefs(func() string { return "" })
	if _, err := r.Hold(context.Background(), "vol-1"); err == nil || errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestUnsafeVolumeIDsAreRejectedWithoutACall(t *testing.T) {
	m := newFakeMaster(t)
	r, _ := newRefs(t, m)
	for _, id := range []string{"", "../x", "a/b", "a b", "a?b=c", "a\x00b"} {
		if _, err := r.Hold(context.Background(), id); !errors.Is(err, ErrVolumeNotFound) {
			t.Errorf("hold %q: %v", id, err)
		}
	}
	if m.acquires.Load() != 0 {
		t.Fatalf("CubeMaster was called %d times for unsafe IDs", m.acquires.Load())
	}
}

// While a volume is in use, an outage must not disturb new requests for it,
// and the reference is still given back once CubeMaster returns.
func TestInUseVolumeSurvivesAnOutage(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	r, _ := newRefs(t, m)

	first := mustHold(t, r, "vol-1")
	m.down.Store(true)
	second := mustHold(t, r, "vol-1") // needs no CubeMaster call
	first()
	second() // last one out: the release cannot be delivered

	if m.refcount("vol-1") != 1 {
		t.Fatalf("refcount = %d; the undelivered release must be kept, not dropped", m.refcount("vol-1"))
	}
	m.down.Store(false)
	eventually(t, "the release to be retried", func() bool { return m.refcount("vol-1") == 0 })
	time.Sleep(50 * time.Millisecond)
	if m.appliedRelease.Load() != 1 {
		t.Fatalf("release applied %d times, want exactly 1", m.appliedRelease.Load())
	}
}

// A release CubeMaster refused is retried until it is applied, once.
func TestRejectedReleaseIsRetriedUntilApplied(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	m.rejectRelease.Store(true)
	r, _ := newRefs(t, m)

	mustHold(t, r, "vol-1")()
	eventually(t, "retries", func() bool { return m.releases.Load() >= 3 })
	if m.refcount("vol-1") != 1 {
		t.Fatal("reference vanished although every release was refused")
	}
	m.rejectRelease.Store(false)
	eventually(t, "the release to be applied", func() bool { return m.refcount("vol-1") == 0 })
	if m.appliedRelease.Load() != 1 {
		t.Fatalf("release applied %d times, want exactly 1", m.appliedRelease.Load())
	}
}

// A request that arrives while a release is being retried takes over the
// reference: no second reference is taken, and the retry stands down.
func TestNewRequestReusesAReferenceThatIsBeingReleased(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	m.rejectRelease.Store(true)
	r, _ := newRefs(t, m)

	mustHold(t, r, "vol-1")()
	eventually(t, "a retry", func() bool { return m.releases.Load() >= 2 })

	again := mustHold(t, r, "vol-1")
	if m.acquires.Load() != 1 || m.refcount("vol-1") != 1 {
		t.Fatalf("acquires=%d refcount=%d, want the existing reference reused", m.acquires.Load(), m.refcount("vol-1"))
	}
	m.rejectRelease.Store(false)
	time.Sleep(50 * time.Millisecond)
	if m.refcount("vol-1") != 1 {
		t.Fatal("retry released a reference that a request was using")
	}
	again()
	if m.refcount("vol-1") != 0 {
		t.Fatalf("refcount = %d after the last request", m.refcount("vol-1"))
	}
}

// If a release may or may not have been applied, repeating it could take away
// a reference that belongs to someone else. It must not be repeated.
func TestReleaseWithUnknownOutcomeIsNotRepeated(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	r, _ := newRefs(t, m)

	m.sandboxRef("vol-1", 1) // a sandbox elsewhere also uses the volume
	rel := mustHold(t, r, "vol-1")
	m.loseRelease.Store(true)
	rel()
	time.Sleep(100 * time.Millisecond)

	if m.releases.Load() != 1 {
		t.Fatalf("release was sent %d times, want 1", m.releases.Load())
	}
	if m.refcount("vol-1") != 1 {
		t.Fatalf("refcount = %d, want 1: the sandbox's reference must survive", m.refcount("vol-1"))
	}

	// And the volume is usable again afterwards.
	m.loseRelease.Store(false)
	mustHold(t, r, "vol-1")()
	if m.refcount("vol-1") != 1 {
		t.Fatalf("refcount = %d, want 1", m.refcount("vol-1"))
	}
}

// If an acquire may or may not have been applied the request fails, and no
// compensating release is sent: a leaked reference only delays a delete, a
// stray release could allow deleting a volume that is in use.
func TestAcquireWithUnknownOutcomeIsNotCompensated(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	m.loseAcquire.Store(true)
	r, _ := newRefs(t, m)

	if _, err := r.Hold(context.Background(), "vol-1"); err == nil {
		t.Fatal("hold succeeded without a confirmed reference")
	}
	time.Sleep(50 * time.Millisecond)
	if m.releases.Load() != 0 {
		t.Fatalf("a release was sent %d times for an unconfirmed acquire", m.releases.Load())
	}
}

// Idle volumes are forgotten, so the table stays bounded however many volumes
// the node serves over its lifetime.
func TestIdleVolumesAreForgotten(t *testing.T) {
	m := newFakeMaster(t)
	r, _ := newRefs(t, m)
	for i := 0; i < maxTrackedVolumes+100; i++ {
		id := fmt.Sprintf("vol-%d", i)
		m.addVolume(id)
		mustHold(t, r, id)()
	}
	r.mu.Lock()
	n := len(r.vols)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d idle volumes are still tracked", n)
	}
}

// Volumes that were found missing are remembered only for a moment, and cannot
// grow the table without bound.
func TestMissingVolumesCannotGrowTheTableWithoutBound(t *testing.T) {
	m := newFakeMaster(t)
	r, clk := newRefs(t, m)
	for i := 0; i < maxTrackedVolumes; i++ {
		_, _ = r.Hold(context.Background(), fmt.Sprintf("gone-%d", i))
	}
	clk.advance(missingTTL + time.Second)
	if _, err := r.Hold(context.Background(), "gone-new"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("expired entries must make room: %v", err)
	}
}

func TestCloseStopsReleaseRetries(t *testing.T) {
	m := newFakeMaster(t)
	m.addVolume("vol-1")
	m.rejectRelease.Store(true)
	r, _ := newRefs(t, m)

	mustHold(t, r, "vol-1")()
	eventually(t, "a retry", func() bool { return m.releases.Load() >= 2 })
	r.Close()
	time.Sleep(50 * time.Millisecond)
	seen := m.releases.Load()
	time.Sleep(100 * time.Millisecond)
	if m.releases.Load() != seen {
		t.Fatal("retries continued after Close")
	}
}

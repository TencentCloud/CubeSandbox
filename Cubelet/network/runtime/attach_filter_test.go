// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestRecoveredFiltersAttachInBackgroundOnce(t *testing.T) {
	controller := newRecoverTestController(t)
	adapter := controller.tapAdapter.(*fakeTapDeviceAdapter)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	orphan := &tapDevice{Name: tapName("10.30.0.8"), Index: 8, IP: net.ParseIP("10.30.0.8").To4()}
	adapter.listResult = map[string]*tapDevice{orphan.IP.String(): orphan}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 0 {
		t.Fatalf("AttachFilter during recover = %d, want 0", got)
	}
	ifindexes := controller.recoveredFilterIfindexes()
	if len(ifindexes) != 1 || ifindexes[0] != orphan.Index {
		t.Fatalf("recovered ifindexes = %v", ifindexes)
	}

	controller.attachRecoveredFilters(ifindexes)
	if got := cubevsAdapter.attachFilterCount(); got != 1 {
		t.Fatalf("AttachFilter after background = %d, want 1", got)
	}
	controller.attachRecoveredFilters(ifindexes)
	if got := cubevsAdapter.attachFilterCount(); got != 1 {
		t.Fatalf("second background pass AttachFilter = %d, want 1", got)
	}
}

func TestAcquireTapAttachesUnrecordedFilterAndKeepsDevice(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	entry := mustReadyEntry(t, tapName("10.30.0.9"), 9, "10.30.0.9")
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp("", "cube-test-tap-*")
	if err != nil {
		t.Fatal(err)
	}
	controller.poolTapFD(entry.TapName, file)
	cubevsAdapter.attachFilterErr = errors.New("attach failed")

	if _, _, err := controller.acquireTap("sandbox-attach"); err == nil {
		t.Fatal("acquireTap succeeded with AttachFilter failure")
	}
	if got := cubevsAdapter.attachFilterCount(); got != 1 {
		t.Fatalf("AttachFilter calls = %d, want 1", got)
	}
	if adapter.destroyCount != 0 {
		t.Fatalf("Destroy calls = %d, want 0", adapter.destroyCount)
	}
	if controller.filterAttached(entry.TapIfIndex) {
		t.Fatal("failed AttachFilter was recorded")
	}
	poolState, owner, ok := controller.tapPool.StateByName(entry.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", poolState, owner, ok)
	}
	if !controller.hasPooledTapFD(entry.TapName) {
		t.Fatal("idle fd was not returned to the pool")
	}

	cubevsAdapter.attachFilterErr = nil
	tap, gotEntry, err := controller.acquireTap("sandbox-attach")
	if err != nil {
		t.Fatal(err)
	}
	if tap.Index != entry.TapIfIndex || gotEntry != entry {
		t.Fatalf("acquired tap=%#v entry=%#v", tap, gotEntry)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 2 {
		t.Fatalf("AttachFilter calls after retry = %d, want 2", got)
	}
	if !controller.filterAttached(entry.TapIfIndex) {
		t.Fatal("successful AttachFilter was not recorded")
	}

	if err := controller.tapPool.ReleaseReservation(entry, "sandbox-attach"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.acquireTap("sandbox-again"); err != nil {
		t.Fatal(err)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 2 {
		t.Fatalf("AttachFilter calls after record hit = %d, want 2", got)
	}
}

func TestAcquireCreatedTapDoesNotAttachTwiceUntilDestroy(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)

	tap, entry, err := controller.acquireTap("sandbox-create")
	if err != nil {
		t.Fatal(err)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 0 {
		t.Fatalf("AttachFilter calls after Create = %d, want 0", got)
	}
	if !controller.filterAttached(tap.Index) {
		t.Fatal("Create did not record AttachFilter")
	}
	if err := controller.tapPool.ReleaseReservation(entry, "sandbox-create"); err != nil {
		t.Fatal(err)
	}
	again, againEntry, err := controller.acquireTap("sandbox-create-again")
	if err != nil {
		t.Fatal(err)
	}
	if again.Index != tap.Index {
		t.Fatalf("second acquire ifindex = %d, want %d", again.Index, tap.Index)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 0 {
		t.Fatalf("AttachFilter calls on recorded tap = %d, want 0", got)
	}
	if err := controller.tapPool.ReleaseReservation(againEntry, "sandbox-create-again"); err != nil {
		t.Fatal(err)
	}

	if err := controller.destroyTapIndex(tap.Index); err != nil {
		t.Fatal(err)
	}
	if controller.filterAttached(tap.Index) {
		t.Fatal("Destroy left the attach record")
	}
	if adapter.destroyCount != 1 {
		t.Fatalf("Destroy calls = %d, want 1", adapter.destroyCount)
	}
	if _, _, err := controller.acquireTap("sandbox-after-destroy"); err != nil {
		t.Fatal(err)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 1 {
		t.Fatalf("AttachFilter calls after Destroy = %d, want 1", got)
	}
}

func TestAsyncAttachFilterFailureDoesNotStick(t *testing.T) {
	controller := newCreateTestController(t, nil)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	entry := mustReadyEntry(t, tapName("10.30.0.11"), 11, "10.30.0.11")
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}
	cubevsAdapter.attachFilterErr = errors.New("attach failed")

	controller.attachRecoveredFilters([]int{entry.TapIfIndex})
	if controller.filterAttached(entry.TapIfIndex) {
		t.Fatal("failed background attach was recorded")
	}
	cubevsAdapter.attachFilterErr = nil
	if _, _, err := controller.acquireTap("sandbox-retry"); err != nil {
		t.Fatal(err)
	}
	if got := cubevsAdapter.attachFilterCount(); got != 2 {
		t.Fatalf("AttachFilter calls = %d, want 2", got)
	}
}

func TestStartAsyncAttachFiltersDoesNotWait(t *testing.T) {
	controller := newCreateTestController(t, nil)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	entry := mustReadyEntry(t, tapName("10.30.0.12"), 12, "10.30.0.12")
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}
	cubevsAdapter.attachFilterEntered = make(chan struct{}, 1)
	cubevsAdapter.attachFilterRelease = make(chan struct{})

	returned := make(chan struct{})
	go func() {
		controller.startAsyncAttachFilters()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("startAsyncAttachFilters waited for AttachFilter")
	}
	select {
	case <-cubevsAdapter.attachFilterEntered:
	case <-time.After(time.Second):
		t.Fatal("background AttachFilter did not start")
	}
	close(cubevsAdapter.attachFilterRelease)
}

func TestAttachFiltersWithLimitCapsConcurrency(t *testing.T) {
	controller := newCreateTestController(t, nil)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	cubevsAdapter.attachFilterEntered = make(chan struct{}, 32)
	release := make(chan struct{})
	cubevsAdapter.attachFilterRelease = release

	ifindexes := make([]int, 20)
	for i := range ifindexes {
		ifindexes[i] = i + 1
	}
	done := make(chan struct{})
	go func() {
		controller.attachFiltersWithLimit(ifindexes)
		close(done)
	}()
	for i := 0; i < recoverAttachFilterConcurrency; i++ {
		select {
		case <-cubevsAdapter.attachFilterEntered:
		case <-time.After(2 * time.Second):
			t.Fatalf("worker %d did not start", i)
		}
	}
	select {
	case <-cubevsAdapter.attachFilterEntered:
		t.Fatal("AttachFilter concurrency exceeded the limit")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("limited AttachFilter did not finish")
	}
	if got := cubevsAdapter.attachFilterMaxInFlight(); got != recoverAttachFilterConcurrency {
		t.Fatalf("max in flight = %d, want %d", got, recoverAttachFilterConcurrency)
	}
	if got := cubevsAdapter.attachFilterCount(); got != len(ifindexes) {
		t.Fatalf("AttachFilter calls = %d, want %d", got, len(ifindexes))
	}
}

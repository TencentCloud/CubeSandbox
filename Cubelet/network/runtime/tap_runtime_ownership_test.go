package runtime

import (
	"context"
	"errors"
	"testing"
)

type ownershipTestTapAdapter struct {
	TapDeviceAdapter
	busy       bool
	calls      int
	busyOnCall int
	uncertain  bool
}

func (a *ownershipTestTapAdapter) CheckUnused(string) error {
	a.calls++
	if a.uncertain {
		return errors.New("procfs inspection failed")
	}
	if a.busy || (a.busyOnCall != 0 && a.calls == a.busyOnCall) {
		return errTapOwned
	}
	return nil
}

func TestCleanerRetainsDeletingStateWhileOldShimOwnsRetainedFD(t *testing.T) {
	recorder := &createOrderRecorder{}
	controller, state := newCleanerTestState(t, recorder)
	controller.cleanStepHook = recorder.record
	adapter := &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, busy: true}
	controller.tapAdapter = adapter
	if !controller.hasPooledTapFD(state.TapName) {
		t.Fatal("test must cover the retained-fd fast path")
	}
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err == nil {
		t.Fatal("busy TAP was returned to the free pool")
	}
	if !controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("cleanup lost its durable retry record")
	}
	status, owner, _ := controller.tapPool.StateByName(state.TapName)
	if status != TapPoolCleaning || owner != state.SandboxID || len(recorder.events) != 0 {
		t.Fatalf("busy TAP cleanup changed ownership/datapath: state=%s owner=%s events=%v", status, owner, recorder.events)
	}
	adapter.busy = false // old shim physically exited; maintenance can retry
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err != nil {
		t.Fatal(err)
	}
	status, owner, _ = controller.tapPool.StateByName(state.TapName)
	if status != TapPoolReady || owner != "" {
		t.Fatalf("retry failed to release TAP: state=%s owner=%s", status, owner)
	}
}

func TestCleanerRechecksOwnershipBeforePublishingReady(t *testing.T) {
	controller, state := newCleanerTestState(t, nil)
	adapter := &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter}
	controller.tapAdapter = adapter
	controller.cleanStepHook = func(step string) {
		if step == "runtime_cleaned" {
			adapter.busy = true
		}
	}
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err == nil {
		t.Fatal("final ownership check was skipped for a retained fd")
	}
	status, _, _ := controller.tapPool.StateByName(state.TapName)
	if status != TapPoolCleaning || !controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("ownership changed during cleanup but TAP was published Ready")
	}
}

func TestAcquireQuarantinesLegacyReadyTapStillHeldByOldShim(t *testing.T) {
	controller := newCreateTestController(t, nil)
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "previous-sandbox"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "previous-sandbox"}); err != nil {
		t.Fatal(err)
	}
	controller.tapAdapter = &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, busy: true}
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "new-sandbox"}); err != nil {
		t.Fatalf("fresh TAP fallback failed: %v", err)
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 2 || entries[0].State != TapPoolCleaning || entries[1].State != TapPoolActive {
		t.Fatalf("legacy busy TAP was not quarantined: %v", entries)
	}
	if entries[0].OwnerSandboxID == "new-sandbox" {
		t.Fatal("quarantine retained the requesting sandbox's port ownership identity")
	}
}

func TestAcquireKeepsActiveOwnerAndDoesNotScanItsOwnHandoff(t *testing.T) {
	controller := newCreateTestController(t, nil)
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "existing"}); err != nil {
		t.Fatal(err)
	}
	adapter := &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, busy: true}
	controller.tapAdapter = adapter
	_, entry, err := controller.acquireTap("existing")
	if err != nil || entry.State != TapPoolActive || adapter.calls != 0 {
		t.Fatalf("active owner rejected: entry=%v calls=%d err=%v", entry, adapter.calls, err)
	}
}

func TestAcquirePreservesReadyInventoryOnInspectionFailure(t *testing.T) {
	controller := newCreateTestController(t, nil)
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "previous"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "previous"}); err != nil {
		t.Fatal(err)
	}
	controller.tapAdapter = &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, uncertain: true}
	if _, _, err := controller.acquireTap("next"); err == nil {
		t.Fatal("uncertain ownership must fail closed")
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || entries[0].State != TapPoolReady || entries[0].OwnerSandboxID != "" {
		t.Fatalf("inspection failure consumed Ready inventory: %v", entries)
	}
}

package runtime

import (
	"context"
	"errors"
	"net"
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
	controller.tapAdapter = &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, busyOnCall: 1}
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
	previousName := controller.tapPool.Entries()[0].TapName
	controller.tapAdapter = &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, uncertain: true}
	for i := 0; i < 10; i++ {
		if _, _, err := controller.acquireTap("next"); err == nil {
			t.Fatal("unavailable inspection allocated a new TAP")
		}
	}
	entries := controller.tapPool.Entries()
	state, owner, found := controller.tapPool.StateByName(previousName)
	if len(entries) != 1 || !found || state != TapPoolReady || owner != "" {
		t.Fatalf("inspection failure consumed inventory: %v", entries)
	}
}

func TestAcquireDoesNotAllocateWithoutInspectionAndReadyInventory(t *testing.T) {
	controller := newCreateTestController(t, nil)
	controller.tapAdapter = &ownershipTestTapAdapter{TapDeviceAdapter: controller.tapAdapter, uncertain: true}
	for i := 0; i < 10; i++ {
		if _, _, err := controller.acquireTap("next"); err == nil {
			t.Fatal("inspection failure allocated a fresh TAP")
		}
	}
	if len(controller.tapPool.Entries()) != 0 {
		t.Fatal("inspection failure consumed an address")
	}
}

type legacyOwnedTapAdapter struct {
	*createTestTapDeviceAdapter
	legacy  *tapDevice
	checked string
}

func (a *legacyOwnedTapAdapter) GetByName(name string) (*tapDevice, error) {
	if name == a.legacy.Name {
		return a.legacy, nil
	}
	return nil, nil
}

func (a *legacyOwnedTapAdapter) CheckUnused(name string) error {
	a.checked = name
	return errTapOwned
}

func TestConflictingLegacyTapChecksActualOwnerBeforeDestroy(t *testing.T) {
	controller := newCreateTestController(t, nil)
	ip := net.ParseIP("192.168.0.14")
	names := candidateTapNames(ip.String())
	adapter := &legacyOwnedTapAdapter{
		createTestTapDeviceAdapter: &createTestTapDeviceAdapter{},
		legacy:                     &tapDevice{Name: names[len(names)-1], Index: 42, IP: ip},
	}
	controller.tapAdapter = adapter
	if err := controller.cleanupConflictingTap(ip); !errors.Is(err, errTapOwned) {
		t.Fatalf("expected occupied legacy TAP, got %v", err)
	}
	if adapter.checked != adapter.legacy.Name || adapter.destroyCount != 0 {
		t.Fatalf("legacy ownership bypassed: checked=%s destroyed=%d", adapter.checked, adapter.destroyCount)
	}
}

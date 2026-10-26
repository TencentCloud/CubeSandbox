package runtime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

func TestCleanupTapForReuseDeletesStateBeforeMarkReady(t *testing.T) {
	// The order matters: a TAP must not become Ready until sandbox-specific state
	// and policy have been removed, otherwise a new sandbox could inherit residue.
	recorder := &createOrderRecorder{}
	controller, state := newCleanerTestState(t, recorder)
	controller.cleanStepHook = recorder.record

	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	cleanupPolicyCallsBefore := len(adapter.cleanupPolicyCalls)
	defaultDenyCallsBefore := len(adapter.defaultDenyPolicyCalls)
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err != nil {
		t.Fatal(err)
	}
	if got, want := len(adapter.cleanupPolicyCalls), cleanupPolicyCallsBefore+1; got != want {
		t.Fatalf("policy cleanup calls after cleanup = %d, want %d", got, want)
	}
	if got, want := len(adapter.defaultDenyPolicyCalls), defaultDenyCallsBefore+1; got != want {
		t.Fatalf("default deny calls after cleanup = %d, want %d", got, want)
	}
	want := []string{
		"fd_close",
		"cubevs_port_mapping_deleted",
		"cubevs_policy_cleaned",
		"cubevs_tap_metadata_deleted",
		"cubeegress_delete",
		"cubeegress_verify",
		"runtime_cleaned",
		"cubevs_default_deny",
		"default_deny_ready",
		"state_deleted",
		"ready",
	}
	if !reflect.DeepEqual(recorder.events, want) {
		t.Fatalf("events = %#v, want %#v", recorder.events, want)
	}
	if controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("deleting state still exists after cleaner success")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", poolState, owner, ok)
	}
	if _, err := controller.GetTapFile(state.SandboxID, state.TapName); err == nil {
		t.Fatal("GetTapFile succeeded after cleaner returned tap to Ready")
	}
}

func TestCleanupTapForReuseKeepsCleaningOnCubeEgressVerifyFailure(t *testing.T) {
	recorder := &createOrderRecorder{}
	controller, state := newCleanerTestState(t, recorder)
	controller.cubeEgressAdapter.(*fakeCubeEgressAdapter).verifyErr = errors.New("policy still exists")

	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err == nil {
		t.Fatal("expected CubeEgress verify error")
	}
	if !controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("deleting state removed despite CubeEgress verify failure")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolCleaning || owner != state.SandboxID {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Cleaning owned by sandbox", poolState, owner, ok)
	}
}

func TestCleanupTapForReuseKeepsCleaningWhenTapFDStillBusy(t *testing.T) {
	controller, state := newCleanerTestState(t, nil)
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	defaultDenyCallsBefore := len(cubevsAdapter.defaultDenyPolicyCalls)
	tapAdapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	tapAdapter.openErr = errors.New("device or resource busy")
	// With fd retention the reusable probe only runs when no retained fd
	// exists (e.g. recovered after a restart). Drop the retained fd to
	// exercise that path; otherwise the probe is skipped by design.
	controller.dropPooledTapFD(state.TapName)

	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err == nil {
		t.Fatal("expected reusable fd probe error")
	}
	if !controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("deleting state removed despite busy tap fd")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolCleaning || owner != state.SandboxID {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Cleaning owned by sandbox", poolState, owner, ok)
	}
	if got := len(cubevsAdapter.defaultDenyPolicyCalls); got != defaultDenyCallsBefore {
		t.Fatalf("default-deny installed before fd reusable: calls=%d want %d", got, defaultDenyCallsBefore)
	}
	if tapAdapter.destroyCount != 0 {
		t.Fatalf("Destroy calls = %d, want 0 for fd probe failure", tapAdapter.destroyCount)
	}
}

func TestCleanupTapForReuseSkipsDuplicateFinishedTask(t *testing.T) {
	recorder := &createOrderRecorder{}
	controller, state := newCleanerTestState(t, recorder)
	controller.cleanStepHook = recorder.record

	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err != nil {
		t.Fatal(err)
	}
	recorder.events = nil
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileDeleting); err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("duplicate finished cleanup executed steps: %#v", recorder.events)
	}
}

func TestCleanupTapForReuseRejectsNonCleaningEntry(t *testing.T) {
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-active"})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states[resp.SandboxID]
	if state == nil {
		t.Fatal("active state missing")
	}
	if err := controller.cleanupTapForReuse(context.Background(), state, StateFileSuccess); err == nil {
		t.Fatal("expected cleaner to reject Active entry")
	}
	if !controller.store.Exists(resp.SandboxID, StateFileSuccess) {
		t.Fatal("success state removed even though entry was not Cleaning")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolActive || owner != resp.SandboxID {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Active owned by sandbox", poolState, owner, ok)
	}
}

func TestCleanupStateOnlyResidueDeletesStateAndReleasesOwnership(t *testing.T) {
	// State-only cleanup is the recovery case where the state file remains but the
	// live TAP is already gone. It must clean external side effects without adding
	// the missing TAP back to TapPool.
	recorder := &createOrderRecorder{}
	controller, record := newStateOnlyCleanerTestRecord(t, recorder)

	usedBefore := controller.allocator.usedIPNum
	if err := controller.cleanupStateOnlyResidue(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := []string{"cubevs_port_mapping_deleted", "cubevs_policy_cleaned", "cubevs_tap_metadata_deleted", "cubeegress_delete", "cubeegress_verify"}
	if !reflect.DeepEqual(recorder.events, want) {
		t.Fatalf("events = %#v, want %#v", recorder.events, want)
	}
	if controller.store.Exists(record.State.SandboxID, record.Kind) {
		t.Fatal("state file still exists after state-only cleanup")
	}
	if _, err := controller.ports.Reserve("new-owner", record.State.PortMappings, controller.cfg.HostPortBindIP); err != nil {
		t.Fatalf("host port ownership was not released: %v", err)
	}
	controller.ports.ReleaseOwnership("new-owner")
	if _, ok := controller.tapPool.GetByName(record.State.TapName); ok {
		t.Fatal("state-only cleanup created or marked a TapPool entry")
	}
	if controller.allocator.usedIPNum != usedBefore-1 {
		t.Fatalf("usedIPNum = %d, want %d", controller.allocator.usedIPNum, usedBefore-1)
	}
}

func TestCleanupStateOnlyResidueKeepsStateOnCubeEgressVerifyFailure(t *testing.T) {
	controller, record := newStateOnlyCleanerTestRecord(t, nil)
	controller.cubeEgressAdapter.(*fakeCubeEgressAdapter).verifyErr = errors.New("policy still exists")

	if err := controller.cleanupStateOnlyResidue(context.Background(), record); err == nil {
		t.Fatal("expected CubeEgress verify error")
	}
	if !controller.store.Exists(record.State.SandboxID, record.Kind) {
		t.Fatal("state file removed despite state-only cleanup failure")
	}
	if _, err := controller.ports.Reserve("new-owner", record.State.PortMappings, controller.cfg.HostPortBindIP); err == nil {
		t.Fatal("host port ownership released despite state-only cleanup failure")
	}
}

func TestCleanupStateOnlyResidueDoesNotDeleteHostPortReusedByAnotherTap(t *testing.T) {
	controller, record := newStateOnlyCleanerTestRecord(t, nil)
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	adapter.listPortMappings = map[uint16]cubevs.MVMPort{
		uint16(record.State.PortMappings[0].HostPort): {Ifindex: uint32(record.State.TapIfIndex + 100), ListenPort: uint16(record.State.PortMappings[0].ContainerPort)},
	}

	if err := controller.cleanupStateOnlyResidue(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(adapter.deletePortMappings) != 0 {
		t.Fatalf("deleted stale hostPort now owned by another TAP: %#v", adapter.deletePortMappings)
	}
}

func TestCleanupStateOnlyResidueDoesNotDeleteReusedHostPortAfterIfindexReuse(t *testing.T) {
	controller, record := newStateOnlyCleanerTestRecord(t, nil)
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	adapter.listPortMappings = map[uint16]cubevs.MVMPort{
		uint16(record.State.PortMappings[0].HostPort): {Ifindex: uint32(record.State.TapIfIndex), ListenPort: uint16(record.State.PortMappings[0].ContainerPort + 1)},
	}

	if err := controller.cleanupStateOnlyResidue(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(adapter.deletePortMappings) != 0 {
		t.Fatalf("deleted hostPort whose ifindex was reused with another container port: %#v", adapter.deletePortMappings)
	}
}

func TestCleanupTapOnceFailureStaysCleaningForMaintenance(t *testing.T) {
	controller, state := newCleanerTestState(t, nil)
	controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappingErr = errors.New("host port mapping still exists")
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)

	if err := controller.cleanupTapOnce(state, StateFileDeleting, "test"); err == nil {
		t.Fatal("expected cleanup error")
	}

	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolCleaning || owner != state.SandboxID {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Cleaning owned by sandbox", poolState, owner, ok)
	}
	if adapter.destroyCount != 0 {
		t.Fatalf("Destroy calls = %d, want 0 for port mapping residue", adapter.destroyCount)
	}
	if !controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("deleting state removed after port mapping cleanup failure")
	}
	entry, ok := controller.tapPool.GetByName(state.TapName)
	if !ok || entry.RetryCount != 1 || entry.LastError == "" {
		t.Fatalf("entry after cleanup failure = %#v", entry)
	}
}

func TestMaintenanceRetriesCleaningWithoutDestroyingTap(t *testing.T) {
	controller, state := newCleanerTestState(t, nil)
	controller.cubevsAdapter.(*fakeCubeVSAdapter).deleteMetadataErr = errors.New("ip metadata still exists")
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)

	if err := controller.cleanupTapOnce(state, StateFileDeleting, "first"); err == nil {
		t.Fatal("expected first cleanup error")
	}
	controller.cubevsAdapter.(*fakeCubeVSAdapter).deleteMetadataErr = nil
	controller.handleCleaningEntries()

	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready after maintenance retry", poolState, owner, ok)
	}
	if controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("deleting state remains after successful maintenance retry")
	}
	if adapter.destroyCount != 0 {
		t.Fatalf("Destroy calls = %d, want 0", adapter.destroyCount)
	}
}

func newCleanerTestState(t *testing.T, recorder *createOrderRecorder) (*NetworkController, *managedState) {
	// Build a real active network, then stop at the durable deleting/Cleaning
	// handoff so individual cleanup attempts can be exercised directly.
	t.Helper()
	controller := newCreateTestController(t, recorder)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID: "sandbox-cleaner",
		PortMappings: []PortMapping{{
			ContainerPort: 8080,
		}},
		CubeNetworkConfig: &CubeNetworkConfig{Rules: []*EgressRule{{
			Name:   "l7",
			Action: &EgressRuleAction{Allow: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states[resp.SandboxID]
	if state == nil {
		t.Fatal("active state is nil")
	}
	if err := controller.store.MarkDeleting(state.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := controller.beginTapCleanup(state.tap, state.SandboxID); err != nil {
		t.Fatal(err)
	}
	controller.closeRuntimeTapOwnership(state)
	delete(controller.states, state.SandboxID)
	if recorder != nil {
		recorder.events = nil
	}
	return controller, state
}

func newStateOnlyCleanerTestRecord(t *testing.T, recorder *createOrderRecorder) (*NetworkController, *StateRecord) {
	// Build a deleting state file plus external CubeVS/PortBinder side effects but
	// deliberately do not add a live TAP or TapPool entry.
	t.Helper()
	controller := newCreateTestController(t, recorder)
	ip, err := controller.allocator.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedState("sandbox-state-only")
	state.TapName = tapName(ip.String())
	state.TapIfIndex = 55
	state.SandboxIP = ip.String()
	state.PortMappings = []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20080, ContainerPort: 8080}}
	state.CubeNetworkConfig = &CubeNetworkConfig{Rules: []*EgressRule{{
		Name:   "l7",
		Action: &EgressRuleAction{Allow: true},
	}}}
	writeStateAs(t, controller.store, state, StateFileDeleting)
	if err := controller.ports.AssignOwner(state.SandboxID, state.PortMappings); err != nil {
		t.Fatal(err)
	}
	tap := &tapDevice{Index: state.TapIfIndex, Name: state.TapName, IP: ip, PortMappings: append([]PortMapping(nil), state.PortMappings...)}
	if err := controller.applyPortMappings(state.SandboxID, tap); err != nil {
		t.Fatal(err)
	}
	if recorder != nil {
		recorder.events = nil
	}
	record, err := controller.store.LoadAny(state.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return controller, record
}

func TestMaintenanceRetriesNoStateCleaningEntrySynchronously(t *testing.T) {
	controller := newCreateTestController(t, nil)
	entry, err := NewTapPoolEntry(tapName("10.30.0.9"), 9, net.ParseIP("10.30.0.9"), "nostate-z10-30-0-9", TapPoolCleaning)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}

	controller.handleCleaningEntries()

	poolState, owner, ok := controller.tapPool.StateByName(entry.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", poolState, owner, ok)
	}
	if got := controller.tapAdapter.(*createTestTapDeviceAdapter).restoreCount; got != 1 {
		t.Fatalf("Restore calls = %d, want 1 for no-state cleanup", got)
	}
}

func TestMaintenanceRetriesCleaningEntryWithPersistedStateSynchronously(t *testing.T) {
	controller := newCreateTestController(t, nil)
	state := testPersistedState("sandbox-maintenance")
	state.SandboxIP = "10.30.0.8"
	state.TapName = tapName(state.SandboxIP)
	state.TapIfIndex = 8
	state.PortMappings = []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20088, ContainerPort: 8088}}
	writeStateAs(t, controller.store, state, StateFileCreating)

	entry, err := NewTapPoolEntry(state.TapName, state.TapIfIndex, net.ParseIP(state.SandboxIP), state.SandboxID, TapPoolCleaning)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}

	controller.handleCleaningEntries()

	if controller.store.Exists(state.SandboxID, StateFileCreating) {
		t.Fatal("creating state remains after successful maintenance cleanup")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", poolState, owner, ok)
	}
	deleted := controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings
	if len(deleted) != 1 || deleted[0].HostPort != 20088 {
		t.Fatalf("deleted port mappings = %#v", deleted)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 0 || egress.verifyCalls != 0 {
		t.Fatalf("known no-L7 state touched CubeEgress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
}

func TestMaintenancePairsInterruptedMigrationCreatingWithLegacy(t *testing.T) {
	controller := newCreateTestController(t, nil)
	recorder := &createOrderRecorder{}
	controller.cleanStepHook = recorder.record
	state := recoverState("sandbox-migration-cleaning", "10.30.0.18", 18, []PortMapping{{
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20080,
		ContainerPort: 80,
	}})
	writeStateAs(t, controller.store, state, StateFileCreating)
	legacyPath := filepath.Join(controller.legacyStateDir, "sandbox-migration-cleaning.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON(state.SandboxID, state.SandboxIP, state.TapIfIndex)), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := NewTapPoolEntry(state.TapName, state.TapIfIndex, net.ParseIP(state.SandboxIP), state.SandboxID, TapPoolCleaning)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}

	controller.handleCleaningEntries()

	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state remains after maintenance cleanup: %v", err)
	}
	if controller.store.Exists(state.SandboxID, StateFileCreating) {
		t.Fatal("creating intent remains after maintenance cleanup")
	}
	legacyDelete, creatingDelete := -1, -1
	for i, event := range recorder.events {
		switch event {
		case "legacy_state_deleted":
			legacyDelete = i
		case "migration_state_deleted":
			creatingDelete = i
		}
	}
	if legacyDelete < 0 || creatingDelete < 0 || legacyDelete >= creatingDelete {
		t.Fatalf("maintenance cleanup order = %#v, want legacy before creating intent", recorder.events)
	}
}

func TestMaintenanceCleansOrphanDeletingStateOnlySynchronously(t *testing.T) {
	controller, record := newStateOnlyCleanerTestRecord(t, nil)

	controller.handleCleaningEntries()

	if controller.store.Exists(record.State.SandboxID, StateFileDeleting) {
		t.Fatal("orphan deleting state still exists after state-only cleanup")
	}
}

func TestMaintenanceCleansOrphanDeletingStateWithLiveTapSynchronously(t *testing.T) {
	controller := newCreateTestController(t, nil)
	state := testPersistedState("sandbox-orphan-live-tap")
	state.SandboxIP = "10.30.0.9"
	state.TapName = tapName(state.SandboxIP)
	state.TapIfIndex = 9
	state.PortMappings = []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20089, ContainerPort: 8089}}
	writeStateAs(t, controller.store, state, StateFileDeleting)
	controller.cubevsAdapter.(*fakeCubeVSAdapter).listPortMappings = map[uint16]cubevs.MVMPort{
		20089: {Ifindex: uint32(state.TapIfIndex), ListenPort: 8089},
	}
	controller.tapAdapter = &fakeTapDeviceAdapter{listResult: map[string]*tapDevice{
		state.SandboxIP: {Name: state.TapName, Index: state.TapIfIndex, IP: net.ParseIP(state.SandboxIP).To4()},
	}}

	controller.handleCleaningEntries()

	if controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("orphan deleting state remains after successful maintenance cleanup")
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", poolState, owner, ok)
	}
}

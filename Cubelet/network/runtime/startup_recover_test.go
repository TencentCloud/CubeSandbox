package runtime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
)

func TestStartupRecoverClassifiesStateAndHostTaps(t *testing.T) {
	controller := newRecoverTestController(t)

	tmp := recoverState("sandbox-tmp", "10.30.0.2", 2, nil)
	success := recoverState("sandbox-success", "10.30.0.3", 3, []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20081, ContainerPort: 8081}})
	creating := recoverState("sandbox-creating", "10.30.0.4", 4, []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20082, ContainerPort: 8082}})
	deleting := recoverState("sandbox-deleting", "10.30.0.5", 5, []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20083, ContainerPort: 8083}})
	missing := recoverState("sandbox-missing", "10.30.0.6", 6, []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20084, ContainerPort: 8084}})

	writeStateAs(t, controller.store, tmp, StateFileTmp)
	writeStateAs(t, controller.store, success, StateFileSuccess)
	writeStateAs(t, controller.store, creating, StateFileCreating)
	writeStateAs(t, controller.store, deleting, StateFileDeleting)
	writeStateAs(t, controller.store, missing, StateFileSuccess)

	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		success.TapName:      recoverTap(success),
		creating.TapName:     recoverTap(creating),
		deleting.TapName:     recoverTap(deleting),
		tapName("10.30.0.7"): {Name: tapName("10.30.0.7"), Index: 7, IP: net.ParseIP("10.30.0.7").To4()},
	}
	controller.cubevsAdapter.(*fakeCubeVSAdapter).listPortMappings = map[uint16]cubevs.MVMPort{
		20082: {Ifindex: uint32(creating.TapIfIndex), ListenPort: 8082},
		20083: {Ifindex: uint32(deleting.TapIfIndex), ListenPort: 8083},
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}

	if controller.store.Exists(tmp.SandboxID, StateFileTmp) {
		t.Fatal("tmp state was not deleted")
	}
	if state := controller.states[success.SandboxID]; state == nil || state.TapName != success.TapName {
		t.Fatalf("success state not restored Active: %#v", state)
	}
	poolState, owner, ok := controller.tapPool.StateByName(success.TapName)
	if !ok || poolState != TapPoolActive || owner != success.SandboxID {
		t.Fatalf("success tap state=%s owner=%s ok=%v, want Active", poolState, owner, ok)
	}
	file, err := controller.GetTapFile(success.SandboxID, success.TapName)
	if err != nil {
		t.Fatalf("lazy fd handoff failed for recovered success state: %v", err)
	}
	_ = file.Close()
	if _, err := controller.ports.Reserve("new-owner", success.PortMappings, controller.cfg.HostPortBindIP); err == nil {
		t.Fatal("success port ownership was not restored to sandbox owner")
	}

	if got := controller.tapAdapter.(*fakeTapDeviceAdapter).restoreCount; got != 2 {
		t.Fatalf("Restore calls = %d, want recovered success plus no-state cleanup", got)
	}
	for _, cleaned := range []*persistedState{creating, deleting} {
		if controller.store.Exists(cleaned.SandboxID, StateFileCreating) || controller.store.Exists(cleaned.SandboxID, StateFileDeleting) {
			t.Fatalf("cleanup-intent state remains after startup cleanup: %s", cleaned.SandboxID)
		}
		poolState, owner, ok := controller.tapPool.StateByName(cleaned.TapName)
		if !ok || poolState != TapPoolReady || owner != "" {
			t.Fatalf("%s tap state=%s owner=%s ok=%v, want Ready", cleaned.SandboxID, poolState, owner, ok)
		}
	}
	if controller.store.Exists(missing.SandboxID, StateFileSuccess) {
		t.Fatal("state-only success residue remains after startup cleanup")
	}
	if _, err := controller.ports.Reserve("new-owner-missing", missing.PortMappings, controller.cfg.HostPortBindIP); err != nil {
		t.Fatalf("state-only cleanup did not release host port: %v", err)
	}
	controller.ports.ReleaseOwnership("new-owner-missing")

	poolState, owner, ok = controller.tapPool.StateByName(tapName("10.30.0.7"))
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("orphan tap state=%s owner=%s ok=%v, want Ready", poolState, owner, ok)
	}
}

func TestStartupRecoverStagesNoStateTapForCleanupBeforeReady(t *testing.T) {
	controller := newRecoverTestController(t)
	adapter := controller.tapAdapter.(*fakeTapDeviceAdapter)
	orphan := &tapDevice{Name: tapName("10.30.0.8"), Index: 8, IP: net.ParseIP("10.30.0.8").To4()}
	adapter.listResult = map[string]*tapDevice{orphan.IP.String(): orphan}
	controller.cubevsAdapter.(*fakeCubeVSAdapter).listPortMappings = map[uint16]cubevs.MVMPort{
		20088: {Ifindex: uint32(orphan.Index), ListenPort: 8088},
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	poolState, owner, ok := controller.tapPool.StateByName(orphan.Name)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("orphan tap state=%s owner=%s ok=%v after startup cleanup, want Ready", poolState, owner, ok)
	}
	if adapter.restoreCount != 1 {
		t.Fatalf("Restore calls = %d, want 1 for no-state TAP cleanup", adapter.restoreCount)
	}
	if len(controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings) != 1 {
		t.Fatalf("no-state cleanup deleted port mappings = %#v", controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings)
	}
	if _, err := controller.ports.Reserve("new-owner", []PortMapping{{HostPort: 20088, ContainerPort: 8088}}, controller.cfg.HostPortBindIP); err != nil {
		t.Fatalf("cleaned no-state TAP should not reserve host ports: %v", err)
	}
}

func TestStartupRecoverActiveTapOpensRuntimeFDLazily(t *testing.T) {
	controller := newRecoverTestController(t)
	adapter := controller.tapAdapter.(*fakeTapDeviceAdapter)
	adapter.restoreWithoutFD = true
	success := recoverState("sandbox-active-no-fd", "10.30.0.31", 31, nil)
	writeStateAs(t, controller.store, success, StateFileSuccess)
	adapter.listResult = map[string]*tapDevice{success.TapName: recoverTap(success)}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if state := controller.states[success.SandboxID]; state == nil || state.tap == nil || state.tap.File != nil {
		t.Fatalf("recovered state should be Active without runtime fd: %#v", state)
	}
	if adapter.openCount != 0 {
		t.Fatalf("Open calls during recover = %d, want 0", adapter.openCount)
	}
	file, err := controller.GetTapFile(success.SandboxID, success.TapName)
	if err != nil {
		t.Fatalf("lazy GetTapFile after recovery failed: %v", err)
	}
	defer file.Close()
	if adapter.openCount != 1 {
		t.Fatalf("Open calls after lazy handoff = %d, want 1", adapter.openCount)
	}
	if state := controller.states[success.SandboxID]; state == nil || state.tap == nil || state.tap.File == nil {
		t.Fatalf("lazy handoff did not cache runtime fd: %#v", state)
	}
}

func TestStartupRecoverDeletingStatesWithDuplicateStaleHostPortsDoNotFail(t *testing.T) {
	controller := newRecoverTestController(t)
	first := recoverState("sandbox-deleting-a", "10.30.0.41", 41, []PortMapping{
		{HostIP: controller.cfg.HostPortBindIP, HostPort: 20091, ContainerPort: 8081},
	})
	second := recoverState("sandbox-deleting-b", "10.30.0.42", 42, []PortMapping{
		{HostIP: controller.cfg.HostPortBindIP, HostPort: 20091, ContainerPort: 8082},
	})
	writeStateAs(t, controller.store, first, StateFileDeleting)
	writeStateAs(t, controller.store, second, StateFileDeleting)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		first.TapName:  recoverTap(first),
		second.TapName: recoverTap(second),
	}
	controller.cubevsAdapter.(*fakeCubeVSAdapter).listPortMappings = map[uint16]cubevs.MVMPort{}

	if err := controller.recover(); err != nil {
		t.Fatalf("recover should not fail on duplicate stale deleting host ports: %v", err)
	}
	for _, cleaned := range []*persistedState{first, second} {
		if controller.store.Exists(cleaned.SandboxID, StateFileDeleting) {
			t.Fatalf("deleting state remains after startup cleanup: %s", cleaned.SandboxID)
		}
		poolState, owner, ok := controller.tapPool.StateByName(cleaned.TapName)
		if !ok || poolState != TapPoolReady || owner != "" {
			t.Fatalf("%s tap state=%s owner=%s ok=%v, want Ready", cleaned.SandboxID, poolState, owner, ok)
		}
	}
	if _, err := controller.ports.Reserve("new-owner", []PortMapping{{HostPort: 20091, ContainerPort: 9091}}, controller.cfg.HostPortBindIP); err != nil {
		t.Fatalf("stale deleting host port should not be reserved during recover: %v", err)
	}
}

func TestStartupRecoverRejectsCrossSandboxCleanupIntentConflict(t *testing.T) {
	controller := newRecoverTestController(t)
	success := recoverState("sandbox-current", "10.30.0.51", 51, nil)
	stale := recoverState("sandbox-stale-delete", success.SandboxIP, success.TapIfIndex, []PortMapping{{
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20095,
		ContainerPort: 8095,
	}})
	stale.CubeNetworkConfig = &CubeNetworkConfig{Rules: []*EgressRule{{
		Name:   "stale-secret",
		Action: &EgressRuleAction{Allow: true},
	}}}
	writeStateAs(t, controller.store, success, StateFileSuccess)
	writeStateAs(t, controller.store, stale, StateFileDeleting)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		success.SandboxIP: recoverTap(success),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover accepted network identity claimed by different sandboxes")
	}
	if !controller.store.Exists(success.SandboxID, StateFileSuccess) ||
		!controller.store.Exists(stale.SandboxID, StateFileDeleting) {
		t.Fatal("cross-sandbox conflict modified durable state")
	}
	if active := controller.states[success.SandboxID]; active != nil {
		t.Fatalf("cross-sandbox conflict published an active state: %#v", active)
	}
	if deleted := controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings; len(deleted) != 0 {
		t.Fatalf("cross-sandbox conflict deleted CubeVS mappings: %#v", deleted)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 0 || egress.verifyCalls != 0 {
		t.Fatalf("cross-sandbox conflict touched CubeEgress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
}

func TestStartupRecoverSuccessSupersedesSameSandboxCleanupIntent(t *testing.T) {
	controller := newRecoverTestController(t)
	state := recoverState("sandbox-same-lifecycle", "10.30.0.52", 52, nil)
	writeStateAs(t, controller.store, state, StateFileSuccess)
	writeStateAs(t, controller.store, state, StateFileDeleting)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if active := controller.states[state.SandboxID]; active == nil || active.TapName != state.TapName {
		t.Fatalf("winning success state not restored: %#v", active)
	}
	if !controller.store.Exists(state.SandboxID, StateFileSuccess) {
		t.Fatal("winning success state was removed")
	}
	if controller.store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("same-sandbox superseded deleting record was not retired")
	}
	if deleted := controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings; len(deleted) != 0 {
		t.Fatalf("same-sandbox superseded record deleted live CubeVS mappings: %#v", deleted)
	}
}

func TestStartupRecoverRejectsSameSandboxPhaseIdentityMismatch(t *testing.T) {
	controller := newRecoverTestController(t)
	success := recoverState("sandbox-conflicting-lifecycle", "10.30.0.55", 55, nil)
	deleting := recoverState(success.SandboxID, "10.30.0.56", 56, nil)
	writeStateAs(t, controller.store, success, StateFileSuccess)
	writeStateAs(t, controller.store, deleting, StateFileDeleting)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		success.SandboxIP:  recoverTap(success),
		deleting.SandboxIP: recoverTap(deleting),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover accepted different network identities as phases of one sandbox lifecycle")
	}
	if !controller.store.Exists(success.SandboxID, StateFileSuccess) ||
		!controller.store.Exists(deleting.SandboxID, StateFileDeleting) {
		t.Fatal("same-sandbox identity conflict modified durable state")
	}
	if len(controller.states) != 0 {
		t.Fatalf("same-sandbox identity conflict published active states: %#v", controller.states)
	}
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(adapter.deletePortMappings) != 0 || len(adapter.cleanupPolicyCalls) != 0 {
		t.Fatalf("same-sandbox identity conflict touched CubeVS: mappings=%#v policy=%#v", adapter.deletePortMappings, adapter.cleanupPolicyCalls)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 0 || egress.verifyCalls != 0 {
		t.Fatalf("same-sandbox identity conflict touched CubeEgress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
}

func TestStartupRecoverRejectsSameSandboxPhasePayloadMismatch(t *testing.T) {
	controller := newRecoverTestController(t)
	success := recoverState("sandbox-conflicting-payload", "10.30.0.57", 57, nil)
	deleting := clonePersistedState(success)
	deleting.PortMappings = []PortMapping{{
		Protocol:      "tcp",
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20097,
		ContainerPort: 8097,
	}}
	deleting.CubeNetworkConfig = &CubeNetworkConfig{Rules: []*EgressRule{{
		Name:   "must-not-discard",
		Action: &EgressRuleAction{Allow: true},
	}}}
	writeStateAs(t, controller.store, success, StateFileSuccess)
	writeStateAs(t, controller.store, deleting, StateFileDeleting)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		success.SandboxIP: recoverTap(success),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover accepted different cleanup payloads as phases of one sandbox lifecycle")
	}
	if !controller.store.Exists(success.SandboxID, StateFileSuccess) ||
		!controller.store.Exists(deleting.SandboxID, StateFileDeleting) {
		t.Fatal("same-sandbox payload conflict modified durable state")
	}
	if len(controller.states) != 0 {
		t.Fatalf("same-sandbox payload conflict published active states: %#v", controller.states)
	}
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(adapter.deletePortMappings) != 0 || len(adapter.cleanupPolicyCalls) != 0 {
		t.Fatalf("same-sandbox payload conflict touched CubeVS: mappings=%#v policy=%#v", adapter.deletePortMappings, adapter.cleanupPolicyCalls)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 0 || egress.verifyCalls != 0 {
		t.Fatalf("same-sandbox payload conflict touched CubeEgress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
}

func TestStartupRecoverRejectsCrossSandboxConflictHiddenBySameSandboxPhase(t *testing.T) {
	controller := newRecoverTestController(t)
	current := recoverState("sandbox-multi-phase", "10.30.0.53", 53, nil)
	stalePhase := recoverState(current.SandboxID, "10.30.0.54", 54, nil)
	other := recoverState("sandbox-other-owner", stalePhase.SandboxIP, stalePhase.TapIfIndex, nil)
	writeStateAs(t, controller.store, current, StateFileSuccess)
	writeStateAs(t, controller.store, stalePhase, StateFileDeleting)
	writeStateAs(t, controller.store, other, StateFileSuccess)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		current.SandboxIP: recoverTap(current),
		other.SandboxIP:   recoverTap(other),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover missed cross-sandbox conflict hidden behind a same-sandbox transaction phase")
	}
	if !controller.store.Exists(current.SandboxID, StateFileSuccess) ||
		!controller.store.Exists(stalePhase.SandboxID, StateFileDeleting) ||
		!controller.store.Exists(other.SandboxID, StateFileSuccess) {
		t.Fatal("hidden cross-sandbox conflict modified durable state")
	}
	if len(controller.states) != 0 {
		t.Fatalf("hidden cross-sandbox conflict published active states: %#v", controller.states)
	}
	if deleted := controller.cubevsAdapter.(*fakeCubeVSAdapter).deletePortMappings; len(deleted) != 0 {
		t.Fatalf("hidden cross-sandbox conflict deleted CubeVS mappings: %#v", deleted)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 0 || egress.verifyCalls != 0 {
		t.Fatalf("hidden cross-sandbox conflict touched CubeEgress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
}

func TestStartupRecoverRejectsTwoSuccessStatesClaimingSameIdentity(t *testing.T) {
	controller := newRecoverTestController(t)
	first := recoverState("sandbox-success-a", "10.30.0.61", 61, nil)
	second := recoverState("sandbox-success-b", first.SandboxIP, first.TapIfIndex, nil)
	writeStateAs(t, controller.store, first, StateFileSuccess)
	writeStateAs(t, controller.store, second, StateFileSuccess)

	err := controller.recover()
	if err == nil {
		t.Fatal("recover accepted two success states claiming one network identity")
	}
	if !controller.store.Exists(first.SandboxID, StateFileSuccess) || !controller.store.Exists(second.SandboxID, StateFileSuccess) {
		t.Fatal("conflicting success state was modified despite hard conflict")
	}
}

func TestStartupRecoverRejectsMultipleLegacyCompanionsForCleanupIntent(t *testing.T) {
	controller := newRecoverTestController(t)
	state := recoverState("sandbox-duplicate-legacy", "10.30.0.71", 71, nil)
	writeStateAs(t, controller.store, state, StateFileCreating)
	firstPath := filepath.Join(controller.legacyStateDir, "legacy-copy-a.json")
	secondPath := filepath.Join(controller.legacyStateDir, "legacy-copy-b.json")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(legacyStateJSON(state.SandboxID, state.SandboxIP, state.TapIfIndex)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover accepted multiple legacy companions for one cleanup intent")
	}
	if !controller.store.Exists(state.SandboxID, StateFileCreating) {
		t.Fatal("creating intent was modified despite ambiguous legacy companions")
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("legacy companion %s was modified despite conflict: %v", path, err)
		}
	}
}

func TestStartupRecoverRejectsDuplicateLegacyStatesWithoutRuntimeIntent(t *testing.T) {
	controller := newRecoverTestController(t)
	state := recoverState("sandbox-duplicate-legacy-only", "10.30.0.72", 72, nil)
	firstPath := filepath.Join(controller.legacyStateDir, "legacy-only-copy-a.json")
	secondPath := filepath.Join(controller.legacyStateDir, "legacy-only-copy-b.json")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(legacyStateJSON(state.SandboxID, state.SandboxIP, state.TapIfIndex)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover accepted duplicate legacy states without a runtime intent")
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("legacy state %s was modified despite conflict: %v", path, err)
		}
	}
	if controller.store.Exists(state.SandboxID, StateFileCreating) ||
		controller.store.Exists(state.SandboxID, StateFileSuccess) {
		t.Fatal("recover wrote runtime state despite duplicate legacy conflict")
	}
	if active := controller.states[state.SandboxID]; active != nil {
		t.Fatalf("duplicate legacy conflict published an active state: %#v", active)
	}
}

func TestStartupRecoverRejectsLegacyPolicyThatDiffersFromCommittedSuccess(t *testing.T) {
	controller := newRecoverTestController(t)
	state := recoverState("sandbox-legacy-success-conflict", "10.30.0.73", 73, []PortMapping{{
		Protocol:      "tcp",
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20080,
		ContainerPort: 80,
	}})
	deny := false
	state.CubeNetworkConfig = &CubeNetworkConfig{AllowInternetAccess: &deny}
	writeStateAs(t, controller.store, state, StateFileSuccess)
	legacyPath := filepath.Join(controller.legacyStateDir, state.SandboxID+".json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON(state.SandboxID, state.SandboxIP, state.TapIfIndex)), 0o600); err != nil {
		t.Fatal(err)
	}
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover retired a legacy policy that differs from committed success")
	}
	if !controller.store.Exists(state.SandboxID, StateFileSuccess) {
		t.Fatal("committed success was modified despite legacy policy conflict")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy policy evidence was modified despite conflict: %v", err)
	}
	if len(controller.states) != 0 {
		t.Fatalf("legacy policy conflict published active states: %#v", controller.states)
	}
}

func newRecoverTestController(t *testing.T) *NetworkController {
	t.Helper()
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := newIPAllocator("10.30.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	ports, err := newPortBinder()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewTapPool()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newNetworkControllerFromDeps(createTestConfig(), networkControllerDeps{
		store:             store,
		allocator:         allocator,
		ports:             ports,
		tapAdapter:        &fakeTapDeviceAdapter{listResult: map[string]*tapDevice{}},
		cubevsAdapter:     &fakeCubeVSAdapter{},
		cubeEgressAdapter: &fakeCubeEgressAdapter{configured: true},
		tapPool:           pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.legacyStateDir = t.TempDir()
	controller.cubeDev = &systemnet.CubeDev{Index: 100}
	return controller
}

func recoverState(sandboxID, ip string, ifindex int, mappings []PortMapping) *persistedState {
	state := testPersistedState(sandboxID)
	state.SandboxIP = ip
	state.TapName = tapName(ip)
	state.TapIfIndex = ifindex
	state.PortMappings = append([]PortMapping(nil), mappings...)
	return state
}

func recoverTap(state *persistedState) *tapDevice {
	return &tapDevice{Name: state.TapName, Index: state.TapIfIndex, IP: net.ParseIP(state.SandboxIP).To4()}
}

func TestLegacyStateScannerReadsLegacyStateWithoutWritingSuccess(t *testing.T) {
	oldDir := t.TempDir()
	legacyPath := filepath.Join(oldDir, "legacy-sandbox.json")
	legacy := legacyStateJSON("legacy-sandbox", "10.0.0.9", 9)
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := (legacyStateScanner{LegacyDir: oldDir}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !records[0].Legacy || records[0].Path != legacyPath || records[0].Fingerprint == "" {
		t.Fatalf("records = %#v", records)
	}
	if records[0].State.SandboxIP != "10.0.0.9" || records[0].State.CubeNetworkConfig == nil {
		t.Fatalf("state = %#v", records[0].State)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy state should remain before recover decision: %v", err)
	}
}

func TestStartupRecoverRestoresLegacyStateAfterReplayAndDeletesLegacyFile(t *testing.T) {
	controller := newRecoverTestController(t)
	oldDir := t.TempDir()
	controller.legacyStateDir = oldDir
	legacyPath := filepath.Join(oldDir, "legacy-sandbox.json")
	legacy := legacyStateJSON("legacy-sandbox", "10.30.0.9", 9)
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	state := recoverState("legacy-sandbox", "10.30.0.9", 9, []PortMapping{{HostIP: controller.cfg.HostPortBindIP, HostPort: 20080, ContainerPort: 80}})
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{state.SandboxIP: recoverTap(state)}
	controller.cubevsAdapter.(*fakeCubeVSAdapter).listPortMappings = map[uint16]cubevs.MVMPort{
		20081: {Ifindex: uint32(state.TapIfIndex), ListenPort: 81},
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file should be deleted after active restore: %v", err)
	}
	if restored := controller.states["legacy-sandbox"]; restored == nil || restored.TapName != state.TapName {
		t.Fatalf("legacy state not restored Active: %#v", restored)
	}
	if _, err := controller.store.Load("legacy-sandbox", StateFileSuccess); err != nil {
		t.Fatalf("new success state missing after legacy restore: %v", err)
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolActive || owner != "legacy-sandbox" {
		t.Fatalf("legacy tap state=%s owner=%s ok=%v, want Active", poolState, owner, ok)
	}
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(adapter.upsertTAPDeviceCalls) != 1 || adapter.upsertTAPDeviceCalls[0] != uint32(state.TapIfIndex) {
		t.Fatalf("upsertTAPDeviceCalls = %#v", adapter.upsertTAPDeviceCalls)
	}
	if len(adapter.deletePortMappings) != 1 || adapter.deletePortMappings[0].HostPort != 20081 {
		t.Fatalf("deletePortMappings = %#v", adapter.deletePortMappings)
	}
	if len(adapter.addPortMappings) != 1 || adapter.addPortMappings[0].HostPort != 20080 {
		t.Fatalf("addPortMappings = %#v", adapter.addPortMappings)
	}
}

func TestLegacyMigrationPreSuccessFailureDeletesLegacyBeforeCreatingIntent(t *testing.T) {
	controller := newRecoverTestController(t)
	recorder := &createOrderRecorder{}
	controller.cleanStepHook = recorder.record
	legacyPath := filepath.Join(controller.legacyStateDir, "legacy-precommit.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON("legacy-precommit", "10.30.0.19", 19)), 0o600); err != nil {
		t.Fatal(err)
	}
	state := recoverState("legacy-precommit", "10.30.0.19", 19, []PortMapping{{
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20080,
		ContainerPort: 80,
	}})
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}
	if err := controller.ports.AssignOwner("conflicting-owner", state.PortMappings); err != nil {
		t.Fatal(err)
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state remains after failed migration cleanup: %v", err)
	}
	if controller.store.Exists(state.SandboxID, StateFileCreating) ||
		controller.store.Exists(state.SandboxID, StateFileSuccess) {
		t.Fatal("new runtime state remains after failed migration cleanup")
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
		t.Fatalf("cleanup order = %#v, want legacy before creating intent", recorder.events)
	}
	poolState, owner, ok := controller.tapPool.StateByName(state.TapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("legacy tap state=%s owner=%s ok=%v, want Ready after rollback", poolState, owner, ok)
	}
}

func TestStartupRecoverPairsInterruptedMigrationCreatingWithLegacy(t *testing.T) {
	controller := newRecoverTestController(t)
	recorder := &createOrderRecorder{}
	controller.cleanStepHook = recorder.record
	legacyPath := filepath.Join(controller.legacyStateDir, "legacy-interrupted.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON("legacy-interrupted", "10.30.0.29", 29)), 0o600); err != nil {
		t.Fatal(err)
	}
	state := recoverState("legacy-interrupted", "10.30.0.29", 29, []PortMapping{{
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20080,
		ContainerPort: 80,
	}})
	writeStateAs(t, controller.store, state, StateFileCreating)
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state remains after interrupted migration cleanup: %v", err)
	}
	if controller.store.Exists(state.SandboxID, StateFileCreating) {
		t.Fatal("creating intent remains after interrupted migration cleanup")
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
		t.Fatalf("recovery cleanup order = %#v, want legacy before creating intent", recorder.events)
	}
}

func TestStartupRecoverStateOnlyInterruptedMigrationTreatsEgressAsUnknown(t *testing.T) {
	controller := newRecoverTestController(t)
	legacyPath := filepath.Join(controller.legacyStateDir, "legacy-state-only.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON("legacy-state-only", "10.30.0.34", 34)), 0o600); err != nil {
		t.Fatal(err)
	}
	state := recoverState("legacy-state-only", "10.30.0.34", 34, nil)
	state.CubeNetworkConfig = nil
	writeStateAs(t, controller.store, state, StateFileCreating)

	if err := controller.recover(); err != nil {
		t.Fatal(err)
	}
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 1 || egress.verifyCalls != 1 {
		t.Fatalf("state-only migration must delete unknown legacy egress: delete=%d verify=%d", egress.deleteCalls, egress.verifyCalls)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state remains after state-only migration cleanup: %v", err)
	}
	if controller.store.Exists(state.SandboxID, StateFileCreating) {
		t.Fatal("creating intent remains after state-only migration cleanup")
	}
}

func TestLegacyMigrationPostSuccessFailureDoesNotCleanCommittedTap(t *testing.T) {
	controller := newRecoverTestController(t)
	legacyPath := filepath.Join(controller.legacyStateDir, "legacy-committed.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON("legacy-committed", "10.30.0.39", 39)), 0o600); err != nil {
		t.Fatal(err)
	}
	state := recoverState("legacy-committed", "10.30.0.39", 39, []PortMapping{{
		HostIP:        controller.cfg.HostPortBindIP,
		HostPort:      20080,
		ContainerPort: 80,
	}})
	controller.tapAdapter.(*fakeTapDeviceAdapter).listResult = map[string]*tapDevice{
		state.SandboxIP: recoverTap(state),
	}
	conflict, err := NewReadyTapPoolEntry(state.TapName, state.TapIfIndex, net.ParseIP(state.SandboxIP))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.tapPool.Add(conflict); err != nil {
		t.Fatal(err)
	}

	if err := controller.recover(); err == nil {
		t.Fatal("recover succeeded despite post-success TapPool publication failure")
	}
	if !controller.store.Exists(state.SandboxID, StateFileSuccess) {
		t.Fatal("committed success was removed after post-success failure")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy state should remain for next-start winner retirement: %v", err)
	}
	adapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(adapter.cleanupPolicyCalls) != 0 || len(adapter.deletedTAPDevices) != 0 {
		t.Fatalf("committed TAP was cleaned after publication failure: policy=%#v metadata=%#v", adapter.cleanupPolicyCalls, adapter.deletedTAPDevices)
	}
}

func TestEnsureNetworkRejectsPendingLegacyLifecycle(t *testing.T) {
	controller := newRecoverTestController(t)
	legacyPath := filepath.Join(controller.legacyStateDir, "legacy-pending.json")
	if err := os.WriteFile(legacyPath, []byte(legacyStateJSON("legacy-pending", "10.30.0.19", 19)), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "legacy-pending"}); err == nil {
		t.Fatal("EnsureNetwork created a new generation while legacy cleanup was pending")
	}
	if controller.store.Exists("legacy-pending", StateFileCreating) || controller.store.Exists("legacy-pending", StateFileSuccess) {
		t.Fatal("EnsureNetwork wrote new state despite pending legacy lifecycle")
	}
}

func TestLegacyStateScannerSkipsCorruptFileAndKeepsItForInspection(t *testing.T) {
	oldDir := t.TempDir()
	legacyPath := filepath.Join(oldDir, "bad-sandbox.json")
	if err := os.WriteFile(legacyPath, []byte(`{"sandboxID":"bad-sandbox",`), 0o644); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(oldDir, "valid-sandbox.json")
	if err := os.WriteFile(validPath, []byte(legacyStateJSON("valid-sandbox", "10.30.0.10", 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := (legacyStateScanner{LegacyDir: oldDir}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State.SandboxID != "valid-sandbox" {
		t.Fatalf("records=%#v, want only valid legacy state", records)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("bad legacy state should remain for inspection: %v", err)
	}
}

func legacyStateJSON(sandboxID, ip string, ifindex int) string {
	return `{
		"sandboxID": "` + sandboxID + `",
		"networkHandle": "` + sandboxID + `",
		"tapName": "` + tapName(ip) + `",
		"tapIfIndex": ` + strconv.Itoa(ifindex) + `,
		"sandboxIP": "` + ip + `",
		"portMappings": [{"protocol":"tcp","hostIP":"127.0.0.1","hostPort":20080,"containerPort":80}],
		"cubevsContext": {"allowInternetAccess": true},
		"persistMetadata": {"host_tap_name":"` + tapName(ip) + `"}
	}`
}

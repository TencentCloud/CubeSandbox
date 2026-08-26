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
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
)

type createOrderRecorder struct {
	events []string
}

// record stores synthetic crash-point events emitted by fake adapters and hooks.
func (r *createOrderRecorder) record(event string) {
	r.events = append(r.events, event)
}

// eventsFrom drops setup noise and returns the suffix beginning at start.
func eventsFrom(events []string, start string) []string {
	for i, event := range events {
		if event == start {
			return events[i:]
		}
	}
	return nil
}

// createTestTapDeviceAdapter creates temporary files as stand-ins for TAP fds so
// controller tests can exercise fd ownership without privileged devices.
type createTestTapDeviceAdapter struct {
	nextIndex    int
	restoreCount int
	openCount    int
	openErr      error
	closeCount   int
	destroyCount int
	destroyErr   error
}

func (f *createTestTapDeviceAdapter) Create(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
	f.nextIndex++
	file, err := os.CreateTemp("", "cube-test-tap-*")
	if err != nil {
		return nil, err
	}
	return &tapDevice{Name: tapName(ip.String()), Index: f.nextIndex, IP: append(net.IP(nil), ip.To4()...), File: file}, nil
}

func (f *createTestTapDeviceAdapter) Restore(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
	f.restoreCount++
	if tap != nil && tap.File == nil {
		file, err := os.CreateTemp("", "cube-test-tap-*")
		if err != nil {
			return nil, err
		}
		tap.File = file
	}
	return tap, nil
}

func (f *createTestTapDeviceAdapter) Open(_ string) (*os.File, error) {
	f.openCount++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return os.CreateTemp("", "cube-test-tap-*")
}

func (f *createTestTapDeviceAdapter) Close(file *os.File) {
	if file != nil {
		f.closeCount++
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
}

func (f *createTestTapDeviceAdapter) List() (map[string]*tapDevice, error) {
	return map[string]*tapDevice{}, nil
}

func (f *createTestTapDeviceAdapter) GetByName(_ string) (*tapDevice, error) {
	return nil, nil
}

func (f *createTestTapDeviceAdapter) Destroy(_ int) error {
	f.destroyCount++
	return f.destroyErr
}

func TestEnsureNetworkCommitsInDocumentedOrder(t *testing.T) {
	// This test documents the create transaction boundary: state reaches creating
	// before external side effects, success precedes Active, and fd handoff only
	// becomes available after that transaction.
	recorder := &createOrderRecorder{}
	controller := newCreateTestController(t, recorder)
	controller.createStepHook = recorder.record
	recorder.events = nil

	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID: "sandbox-order",
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
	if len(resp.PortMappings) != 1 || resp.PortMappings[0].HostPort == 0 {
		t.Fatalf("PortMappings = %#v", resp.PortMappings)
	}

	want := []string{"creating", "cubevs_port_mapping", "cubevs_tap", "cubeegress_put", "pre_success", "success", "active"}
	got := eventsFrom(recorder.events, "creating")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", recorder.events, want)
	}

	if controller.store.Exists("sandbox-order", StateFileCreating) {
		t.Fatal("creating state still exists after successful create")
	}
	persisted, err := controller.store.Load("sandbox-order", StateFileSuccess)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.PortMappings) != 1 || persisted.PortMappings[0].HostPort != resp.PortMappings[0].HostPort {
		t.Fatalf("persisted PortMappings = %#v, response = %#v", persisted.PortMappings, resp.PortMappings)
	}
	file, err := controller.GetTapFile("sandbox-order", resp.Interfaces[0].Name)
	if err != nil {
		t.Fatalf("fd should be available after success and Active: %v", err)
	}
	_ = file.Close()
}

func TestEnsureNetworkPreCreatingFailureReleasesReservationOnly(t *testing.T) {
	controller := newCreateTestController(t, nil)
	_, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "invalid/sandbox"})
	if err == nil {
		t.Fatal("expected invalid sandboxID error")
	}
	if records, err := controller.store.Scan(); err != nil {
		t.Fatal(err)
	} else if len(records) != 0 {
		t.Fatalf("state records = %#v, want none", records)
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 {
		t.Fatalf("tap pool entries = %d, want 1", len(entries))
	}
	if entries[0].State != TapPoolReady || entries[0].OwnerSandboxID != "" {
		t.Fatalf("entry after pre-creating failure = %#v, want Ready without owner", entries[0])
	}
}

func TestEnsureNetworkCommitSuccessErrorPreservesObservedDurableSuccess(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	cleanSteps := &createOrderRecorder{}
	controller.cleanStepHook = cleanSteps.record
	controller.createStepHook = func(step string) {
		if step == "pre_success" {
			if err := controller.store.CommitSuccess("sandbox-success-ambiguous"); err != nil {
				t.Fatalf("pre-commit success state: %v", err)
			}
		}
	}

	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-success-ambiguous"}); err == nil {
		t.Fatal("EnsureNetwork succeeded despite ambiguous success commit result")
	}
	if !controller.store.Exists("sandbox-success-ambiguous", StateFileSuccess) {
		t.Fatal("observed durable success was removed by create rollback")
	}
	if len(cleanSteps.events) != 0 {
		t.Fatalf("post-success failure triggered destructive cleanup: %#v", cleanSteps.events)
	}
	// With fd retention the runtime fd is released to the idle pool registry,
	// not closed.
	if adapter.closeCount != 0 {
		t.Fatalf("runtime-owned TAP fd close count = %d, want 0 (fd retained in pool registry)", adapter.closeCount)
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || !controller.hasPooledTapFD(entries[0].TapName) {
		t.Fatalf("released TAP fd not retained in pool registry: entries=%#v", entries)
	}
}

func TestEnsureNetworkActivePublicationFailurePreservesSuccess(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	cleanSteps := &createOrderRecorder{}
	controller.cleanStepHook = cleanSteps.record
	controller.createStepHook = func(step string) {
		if step != "success" {
			return
		}
		entries := controller.tapPool.Entries()
		if len(entries) != 1 {
			t.Fatalf("tap pool entries=%d, want 1", len(entries))
		}
		if _, err := controller.tapPool.BeginCleanupByName(entries[0].TapName, "sandbox-active-publish-fail"); err != nil {
			t.Fatalf("force Active publication failure: %v", err)
		}
	}

	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-active-publish-fail"}); err == nil {
		t.Fatal("EnsureNetwork succeeded despite Active publication failure")
	}
	if !controller.store.Exists("sandbox-active-publish-fail", StateFileSuccess) {
		t.Fatal("durable success was removed after Active publication failure")
	}
	if len(cleanSteps.events) != 0 {
		t.Fatalf("Active publication failure triggered destructive cleanup: %#v", cleanSteps.events)
	}
	// With fd retention the runtime fd is released to the idle pool registry,
	// not closed.
	if adapter.closeCount != 0 {
		t.Fatalf("runtime-owned TAP fd close count = %d, want 0 (fd retained in pool registry)", adapter.closeCount)
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || !controller.hasPooledTapFD(entries[0].TapName) {
		t.Fatalf("released TAP fd not retained in pool registry: entries=%#v", entries)
	}
	controller.handleCleaningEntries()
	if !controller.store.Exists("sandbox-active-publish-fail", StateFileSuccess) {
		t.Fatal("maintenance removed success after Active publication failure")
	}
	if len(cleanSteps.events) != 0 {
		t.Fatalf("maintenance cleaned committed success: %#v", cleanSteps.events)
	}
}

func TestEnsureNetworkPostCreatingFailureCleansSynchronously(t *testing.T) {
	// After CommitCreating, create failure performs one synchronous cleanup.
	// When that attempt succeeds, no durable residue is left for maintenance.
	controller := newCreateTestController(t, nil)
	controller.cubeEgressAdapter.(*fakeCubeEgressAdapter).putErr = errors.New("cubeegress unavailable")

	_, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID: "sandbox-create-fail",
		PortMappings: []PortMapping{{
			ContainerPort: 8080,
		}},
		CubeNetworkConfig: &CubeNetworkConfig{Rules: []*EgressRule{{
			Name:   "l7",
			Action: &EgressRuleAction{Allow: true},
		}}},
	})
	if err == nil {
		t.Fatal("expected CubeEgress PUT error")
	}
	if controller.store.Exists("sandbox-create-fail", StateFileCreating) {
		t.Fatal("creating state remains after successful synchronous cleanup")
	}
	if controller.store.Exists("sandbox-create-fail", StateFileSuccess) {
		t.Fatal("success state exists after failed create")
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || entries[0].State != TapPoolReady || entries[0].OwnerSandboxID != "" {
		t.Fatalf("tap entries after synchronous create rollback = %#v, want one unowned Ready TAP", entries)
	}
	if _, err := controller.GetTapFile("sandbox-create-fail", entries[0].TapName); err == nil {
		t.Fatal("GetTapFile succeeded for failed create in Cleaning")
	}
}

func TestReleaseNetworkRetriesPendingCreatingLifecycleWhenActiveMapIsMissing(t *testing.T) {
	controller := newCreateTestController(t, nil)
	egress := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	egress.putErr = errors.New("cubeegress unavailable")
	egress.verifyErr = errors.New("old policy still present")

	_, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID: "sandbox-create-pending",
		CubeNetworkConfig: &CubeNetworkConfig{Rules: []*EgressRule{{
			Name:   "l7",
			Action: &EgressRuleAction{Allow: true},
		}}},
	})
	if err == nil {
		t.Fatal("expected create failure")
	}
	if !controller.store.Exists("sandbox-create-pending", StateFileCreating) {
		t.Fatal("creating state missing after synchronous cleanup failure")
	}
	if _, active := controller.states["sandbox-create-pending"]; active {
		t.Fatal("failed create was published in Active map")
	}

	egress.verifyErr = nil
	resp, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-create-pending"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Released {
		t.Fatal("pending creating lifecycle was not released")
	}
	if controller.store.Exists("sandbox-create-pending", StateFileCreating) {
		t.Fatal("creating state remains after explicit release retry")
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || entries[0].State != TapPoolReady || entries[0].OwnerSandboxID != "" {
		t.Fatalf("tap entries after explicit release retry = %#v", entries)
	}
}

func TestEnsureNetworkRetainsFreshRuntimeTapFDWhenActive(t *testing.T) {
	controller := newCreateTestController(t, nil)
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-active"}); err != nil {
		t.Fatal(err)
	}
	state := controller.states["sandbox-active"]
	if state == nil || state.tap == nil {
		t.Fatalf("state tap not initialized: %#v", state)
	}
	if state.tap.File == nil {
		t.Fatal("runtime dropped fresh creation fd after Active")
	}
}

func TestGetTapFileDoesNotRestoreOnReopenFailure(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-fd"})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states["sandbox-fd"]
	if state == nil || state.tap == nil || state.tap.File == nil {
		t.Fatalf("state tap should retain fresh fd after Active: %#v", state)
	}
	controller.closeRuntimeTapFile(state.tap)
	adapter.openErr = errors.New("open failed")

	if _, err := controller.GetTapFile("sandbox-fd", resp.Interfaces[0].Name); err == nil {
		t.Fatal("GetTapFile succeeded despite reopen failure")
	}
	if adapter.openCount != 1 {
		t.Fatalf("Open calls = %d, want 1", adapter.openCount)
	}
	if adapter.restoreCount != 0 {
		t.Fatalf("Restore calls = %d, want 0", adapter.restoreCount)
	}
}

func TestPoolRetainedTapFDSkipsHandoffOpenAcrossLifecycles(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)

	// Warm the pool: the creation fd is retained in the idle registry.
	if err := controller.createPoolTap(); err != nil {
		t.Fatal(err)
	}
	entries := controller.tapPool.Entries()
	if len(entries) != 1 || entries[0].State != TapPoolReady {
		t.Fatalf("pool entries = %#v", entries)
	}
	if !controller.hasPooledTapFD(entries[0].TapName) {
		t.Fatal("creation fd not retained for pooled tap")
	}

	// First lifecycle: acquire from pool; the handoff must not Open.
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-pool-1"})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states["sandbox-pool-1"]
	if state == nil || state.tap == nil || state.tap.File == nil {
		t.Fatalf("pool-acquired tap lacks retained fd: %#v", state)
	}
	if _, err := controller.GetTapFile("sandbox-pool-1", resp.Interfaces[0].Name); err != nil {
		t.Fatal(err)
	}
	if adapter.openCount != 0 {
		t.Fatalf("Open calls = %d, want 0 for retained pool fd", adapter.openCount)
	}

	// Release: the fd returns to the registry for the next lifecycle.
	if _, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-pool-1"}); err != nil {
		t.Fatal(err)
	}
	if !controller.hasPooledTapFD(entries[0].TapName) {
		t.Fatal("released tap fd not returned to pool registry")
	}

	// Second lifecycle on the same tap: still no Open, and the fd was never
	// closed in between.
	if _, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-pool-2"}); err != nil {
		t.Fatal(err)
	}
	state2 := controller.states["sandbox-pool-2"]
	if state2 == nil || state2.tap == nil || state2.tap.File == nil {
		t.Fatalf("second-lifecycle tap lacks retained fd: %#v", state2)
	}
	if adapter.openCount != 0 {
		t.Fatalf("Open calls = %d after tap reuse, want 0", adapter.openCount)
	}
	if adapter.closeCount != 0 {
		t.Fatalf("Close calls = %d, want 0 (fd retained across lifecycles)", adapter.closeCount)
	}
}

func TestGetTapFileReturnsCallerOwnedDuplicate(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-fd-dup"})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states["sandbox-fd-dup"]
	if state == nil || state.tap == nil || state.tap.File == nil {
		t.Fatalf("state tap should retain fresh fd after Active: %#v", state)
	}
	runtimeFD := state.tap.File.Fd()

	first, err := controller.GetTapFile("sandbox-fd-dup", resp.Interfaces[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fd() == runtimeFD {
		t.Fatalf("GetTapFile returned runtime-owned fd %d instead of duplicate", runtimeFD)
	}
	if adapter.openCount != 0 {
		t.Fatalf("Open calls = %d, want 0 for retained fresh fd", adapter.openCount)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := controller.GetTapFile("sandbox-fd-dup", resp.Interfaces[0].Name)
	if err != nil {
		t.Fatalf("runtime cached fd should survive caller close: %v", err)
	}
	defer second.Close()
	if state.tap.File == nil || state.tap.File.Fd() != runtimeFD {
		t.Fatalf("runtime fd changed after caller close: got=%v want_fd=%d", state.tap.File, runtimeFD)
	}
	if adapter.openCount != 0 {
		t.Fatalf("Open calls after cached handoff = %d, want 0", adapter.openCount)
	}
}

func TestGetTapFileReopensAfterStaleCachedFD(t *testing.T) {
	controller := newCreateTestController(t, nil)
	adapter := controller.tapAdapter.(*createTestTapDeviceAdapter)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-fd-stale"})
	if err != nil {
		t.Fatal(err)
	}
	state := controller.states["sandbox-fd-stale"]
	if state == nil || state.tap == nil || state.tap.File == nil {
		t.Fatalf("state tap should retain fresh fd after Active: %#v", state)
	}
	if err := state.tap.File.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := controller.GetTapFile("sandbox-fd-stale", resp.Interfaces[0].Name)
	if err != nil {
		t.Fatalf("GetTapFile should reopen active tap: %v", err)
	}
	defer file.Close()
	if adapter.openCount != 1 {
		t.Fatalf("Open calls = %d, want 1", adapter.openCount)
	}
}

func newCreateTestController(t *testing.T, recorder *createOrderRecorder) *NetworkController {
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
		tapAdapter:        &createTestTapDeviceAdapter{},
		cubevsAdapter:     &fakeCubeVSAdapter{recorder: recorder},
		cubeEgressAdapter: &fakeCubeEgressAdapter{configured: true, recorder: recorder},
		tapPool:           pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.legacyStateDir = t.TempDir()
	controller.cubeDev = &systemnet.CubeDev{Index: 100}
	return controller
}

func createTestConfig() Config {
	cfg := DefaultConfig()
	cfg.CIDR = "10.30.0.0/24"
	cfg.CubeEgressAdminURL = "http://cubeegress.invalid"
	return cfg
}

func TestMain(m *testing.M) {
	oldEnsureRoute := ensureRouteToCubeDevFunc
	ensureRouteToCubeDevFunc = func(_ string, _ *systemnet.CubeDev) error { return nil }
	code := m.Run()
	ensureRouteToCubeDevFunc = oldEnsureRoute
	os.Exit(code)
}

func TestEnsureHostRouteInstallsOncePerController(t *testing.T) {
	old := ensureRouteToCubeDevFunc
	defer func() { ensureRouteToCubeDevFunc = old }()
	calls := 0
	fail := true
	ensureRouteToCubeDevFunc = func(_ string, _ *systemnet.CubeDev) error {
		calls++
		if fail {
			return errors.New("route install failed")
		}
		return nil
	}
	controller := newCreateTestController(t, nil)
	if err := controller.ensureHostRoute(); err == nil {
		t.Fatal("expected route install failure")
	}
	if err := controller.ensureHostRoute(); err == nil {
		t.Fatal("failure must not latch; expected the retry to run and fail again")
	}
	fail = false
	if err := controller.ensureHostRoute(); err != nil {
		t.Fatal(err)
	}
	if err := controller.ensureHostRoute(); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("route install calls = %d, want 3 (2 failed attempts + 1 success, then cached)", calls)
	}
}

func TestReleaseNetworkCommitsDeleteIntentAndCleansSynchronously(t *testing.T) {
	// Release first commits the deleting intent, then closes runtime fd ownership
	// and removes Active ownership before performing one synchronous cleanup.
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-release"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := controller.GetTapFile("sandbox-release", resp.Interfaces[0].Name)
	if err != nil {
		t.Fatalf("fd should be available before release: %v", err)
	}
	_ = file.Close()

	recorder := &createOrderRecorder{}
	controller.releaseStepHook = recorder.record
	releaseResp, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-release"})
	if err != nil {
		t.Fatal(err)
	}
	if !releaseResp.Released {
		t.Fatalf("Released = false")
	}
	want := []string{"deleting", "cleaning", "fd_close", "active_removed", "cleaned"}
	if !reflect.DeepEqual(recorder.events, want) {
		t.Fatalf("events = %#v, want %#v", recorder.events, want)
	}
	if controller.store.Exists("sandbox-release", StateFileSuccess) {
		t.Fatal("success state still exists after release")
	}
	if controller.store.Exists("sandbox-release", StateFileDeleting) {
		t.Fatal("deleting state remains after successful synchronous release")
	}
	state, owner, ok := controller.tapPool.StateByName(resp.Interfaces[0].Name)
	if !ok || state != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready without owner", state, owner, ok)
	}
	if _, err := controller.GetTapFile("sandbox-release", resp.Interfaces[0].Name); err == nil {
		t.Fatal("GetTapFile succeeded after release entered Cleaning")
	}
}

func TestReleaseNetworkReportsDurablePendingStateOnPoolOwnerMismatch(t *testing.T) {
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-release-failure"})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := controller.tapPool.GetByName(resp.Interfaces[0].Name)
	if !ok {
		t.Fatal("tap pool entry missing")
	}
	entry.OwnerSandboxID = "unexpected-owner"

	releaseResp, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-release-failure"})
	if err == nil {
		t.Fatal("expected cleanup handoff error")
	}
	if !releaseResp.Released {
		t.Fatal("release should remain accepted after deleting intent is committed")
	}
	if !controller.store.Exists("sandbox-release-failure", StateFileDeleting) {
		t.Fatal("deleting state missing after cleanup handoff failure")
	}
	poolState, owner, ok := controller.tapPool.StateByName(resp.Interfaces[0].Name)
	if !ok || poolState != TapPoolActive || owner != "unexpected-owner" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want unchanged inconsistent Active entry", poolState, owner, ok)
	}
}

func TestReleaseNetworkContinuesWhenDeletingStateAlreadyCommitted(t *testing.T) {
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-delete-ambiguous"})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.store.MarkDeleting("sandbox-delete-ambiguous"); err != nil {
		t.Fatal(err)
	}

	releaseResp, err := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-delete-ambiguous"})
	if err == nil {
		t.Fatal("ReleaseNetwork should preserve the unconfirmed deleting durability error")
	}
	if releaseResp == nil || !releaseResp.Released {
		t.Fatalf("ReleaseNetwork response=%+v, want Released after completed cleanup", releaseResp)
	}
	if controller.store.Exists("sandbox-delete-ambiguous", StateFileSuccess) ||
		controller.store.Exists("sandbox-delete-ambiguous", StateFileDeleting) {
		t.Fatal("already-committed deleting lifecycle was not cleaned")
	}
	poolState, owner, ok := controller.tapPool.StateByName(resp.Interfaces[0].Name)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready after deleting cleanup", poolState, owner, ok)
	}
	controller.mu.Lock()
	_, active := controller.states["sandbox-delete-ambiguous"]
	controller.mu.Unlock()
	if active {
		t.Fatal("active state remains after deleting cleanup")
	}
}

func TestReleaseDurableRecordContinuesWhenDeletingStateAlreadyCommitted(t *testing.T) {
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-durable-delete-ambiguous"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := controller.store.LoadAny("sandbox-durable-delete-ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	delete(controller.states, "sandbox-durable-delete-ambiguous")
	controller.mu.Unlock()
	if err := controller.store.MarkDeleting("sandbox-durable-delete-ambiguous"); err != nil {
		t.Fatal(err)
	}

	err = controller.releaseDurableRecord(context.Background(), record)
	if err == nil {
		t.Fatal("releaseDurableRecord should preserve the unconfirmed deleting durability error")
	}
	if controller.store.Exists("sandbox-durable-delete-ambiguous", StateFileSuccess) ||
		controller.store.Exists("sandbox-durable-delete-ambiguous", StateFileDeleting) {
		t.Fatal("already-committed durable deleting lifecycle was not cleaned")
	}
	poolState, owner, ok := controller.tapPool.StateByName(resp.Interfaces[0].Name)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap state=%s owner=%s ok=%v, want Ready after durable deleting cleanup", poolState, owner, ok)
	}
}

func TestNewNetworkControllerFromDepsInjectsRuntimeDependencies(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := newIPAllocator("10.10.0.0/24")
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
	tapAdapter := &fakeTapDeviceAdapter{}
	cubevsAdapter := &fakeCubeVSAdapter{}
	cubeEgressAdapter := &fakeCubeEgressAdapter{}
	locks := NewSandboxLocks()

	controller, err := newNetworkControllerFromDeps(DefaultConfig(), networkControllerDeps{
		store:             store,
		allocator:         allocator,
		ports:             ports,
		tapAdapter:        tapAdapter,
		cubevsAdapter:     cubevsAdapter,
		cubeEgressAdapter: cubeEgressAdapter,
		tapPool:           pool,
		locks:             locks,
	})
	if err != nil {
		t.Fatal(err)
	}
	var _ NetworkRuntime = controller
	if controller.store != store || controller.allocator != allocator || controller.ports != ports {
		t.Fatal("core dependencies were not injected")
	}
	if controller.tapAdapter != tapAdapter || controller.cubevsAdapter != cubevsAdapter || controller.cubeEgressAdapter != cubeEgressAdapter {
		t.Fatal("adapter dependencies were not injected")
	}
	if controller.tapPool != pool || controller.locks != locks {
		t.Fatal("pool or sandbox locks were not injected")
	}
	if controller.states == nil {
		t.Fatal("runtime state map was not initialized")
	}
}

func TestNewNetworkControllerFromDepsRequiresAdapters(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := newIPAllocator("10.20.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	ports, err := newPortBinder()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newNetworkControllerFromDeps(DefaultConfig(), networkControllerDeps{
		store:     store,
		allocator: allocator,
		ports:     ports,
	}); err == nil {
		t.Fatal("expected missing adapter error")
	}
}

func TestLoadL7MarksConfig(t *testing.T) {
	withPath := func(t *testing.T, content *string) {
		t.Helper()
		old := l7MarksConfigPath
		t.Cleanup(func() { l7MarksConfigPath = old })
		if content == nil {
			l7MarksConfigPath = filepath.Join(t.TempDir(), "absent.conf")
			return
		}
		p := filepath.Join(t.TempDir(), "l7-marks.conf")
		if err := os.WriteFile(p, []byte(*content), 0o644); err != nil {
			t.Fatalf("write temp conf: %v", err)
		}
		l7MarksConfigPath = p
	}

	t.Run("absent file leaves defaults", func(t *testing.T) {
		withPath(t, nil)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err != nil {
			t.Fatalf("absent file: %v", err)
		}
		if p.L7MarkHTTP != 0 || p.L7MarkHTTPS != 0 || p.L7MarkMask != 0 {
			t.Fatalf("absent file set marks: %+v", p)
		}
	})

	t.Run("override is applied", func(t *testing.T) {
		conf := "# comment\nCUBE_L7_MARK_HTTP=0xCF010000\nCUBE_L7_MARK_HTTPS=0xCF020000\nCUBE_L7_MARK_MASK=0xFFFF0000\n"
		withPath(t, &conf)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err != nil {
			t.Fatalf("override: %v", err)
		}
		if p.L7MarkHTTP != 0xCF010000 || p.L7MarkHTTPS != 0xCF020000 || p.L7MarkMask != 0xFFFF0000 {
			t.Fatalf("override: got http=%#x https=%#x mask=%#x", p.L7MarkHTTP, p.L7MarkHTTPS, p.L7MarkMask)
		}
	})

	t.Run("partial override keeps others zero", func(t *testing.T) {
		conf := "CUBE_L7_MARK_HTTP=0xCF030000\n"
		withPath(t, &conf)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err != nil {
			t.Fatalf("partial: %v", err)
		}
		if p.L7MarkHTTP != 0xCF030000 || p.L7MarkHTTPS != 0 || p.L7MarkMask != 0 {
			t.Fatalf("partial: got http=%#x https=%#x mask=%#x", p.L7MarkHTTP, p.L7MarkHTTPS, p.L7MarkMask)
		}
	})

	t.Run("malformed value errors", func(t *testing.T) {
		conf := "CUBE_L7_MARK_HTTP=not-a-number\n"
		withPath(t, &conf)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err == nil {
			t.Fatal("malformed value was not rejected")
		}
	})

	t.Run("export-prefixed key is honored", func(t *testing.T) {
		// Shell-legal `export KEY=value` must not be silently ignored —
		// otherwise the dataplane stamps a different mark than iptables
		// matches.
		conf := "export CUBE_L7_MARK_HTTP=0xCF010000\nexport CUBE_L7_MARK_MASK=0xFFFF0000\n"
		withPath(t, &conf)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err != nil {
			t.Fatalf("export-prefixed: %v", err)
		}
		if p.L7MarkHTTP != 0xCF010000 || p.L7MarkMask != 0xFFFF0000 {
			t.Fatalf("export-prefixed: got http=%#x mask=%#x", p.L7MarkHTTP, p.L7MarkMask)
		}
	})

	t.Run("inline comment on value is stripped", func(t *testing.T) {
		conf := "CUBE_L7_MARK_HTTP=0xCF030000 # http listener mark\n"
		withPath(t, &conf)
		var p cubevs.Params
		if err := loadL7MarksConfig(&p); err != nil {
			t.Fatalf("inline comment: %v", err)
		}
		if p.L7MarkHTTP != 0xCF030000 {
			t.Fatalf("inline comment: got http=%#x", p.L7MarkHTTP)
		}
	})
}

// registerActiveSandbox puts a controller into the state UpdateNetworkPolicy
// requires: an in-memory managedState plus a committed success state file.
func registerActiveSandbox(t *testing.T, c *NetworkController, sandboxID string, cfg *CubeNetworkConfig, dnsCIDRs []string) *managedState {
	t.Helper()
	state := &managedState{persistedState: persistedState{
		SandboxID:         sandboxID,
		NetworkHandle:     sandboxID,
		TapName:           "tap-" + sandboxID,
		TapIfIndex:        77,
		SandboxIP:         "10.30.0.7",
		CubeNetworkConfig: cfg,
		DNSAllowOutCIDRs:  dnsCIDRs,
	}}
	if err := c.store.WriteTmp(&state.persistedState); err != nil {
		t.Fatalf("WriteTmp: %v", err)
	}
	if err := c.store.CommitCreating(sandboxID); err != nil {
		t.Fatalf("CommitCreating: %v", err)
	}
	if err := c.store.CommitSuccess(sandboxID); err != nil {
		t.Fatalf("CommitSuccess: %v", err)
	}
	c.states[sandboxID] = state
	return state
}

func TestUpdateNetworkPolicyAppliesAndPersists(t *testing.T) {
	c := newCreateTestController(t, nil)
	state := registerActiveSandbox(t, c, "sb-update", &CubeNetworkConfig{AllowOut: []string{"1.1.1.1/32"}}, nil)

	newCfg := &CubeNetworkConfig{AllowOut: []string{"2.2.2.2/32"}}
	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-update",
		CubeNetworkConfig: newCfg,
	}); err != nil {
		t.Fatalf("UpdateNetworkPolicy: %v", err)
	}

	adapter := c.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(adapter.updatedPolicies) != 1 {
		t.Fatalf("CubeVS update calls=%d, want 1", len(adapter.updatedPolicies))
	}
	if got := adapter.updatedPolicies[0].ifindex; got != 77 {
		t.Errorf("updated ifindex=%d, want 77", got)
	}
	if allow := adapter.updatedPolicies[0].opts.AllowOut; allow == nil || len(*allow) != 1 || (*allow)[0] != "2.2.2.2/32" {
		t.Errorf("CubeVS got allow_out %v, want [2.2.2.2/32]", allow)
	}

	if len(state.CubeNetworkConfig.AllowOut) != 1 || state.CubeNetworkConfig.AllowOut[0] != "2.2.2.2/32" {
		t.Errorf("in-memory state not updated: %v", state.CubeNetworkConfig.AllowOut)
	}
	persisted, err := c.store.Load("sb-update", StateFileSuccess)
	if err != nil {
		t.Fatalf("reload success state: %v", err)
	}
	if persisted.CubeNetworkConfig == nil || len(persisted.CubeNetworkConfig.AllowOut) != 1 ||
		persisted.CubeNetworkConfig.AllowOut[0] != "2.2.2.2/32" {
		t.Errorf("state file not rewritten: %+v", persisted.CubeNetworkConfig)
	}
}

// TestUpdateNetworkPolicyRefoldsDNSResolvers guards the failure mode that would
// break every domain rule: an update carries only user targets, so the resolver
// CIDRs recorded at create must be folded back in.
func TestUpdateNetworkPolicyRefoldsDNSResolvers(t *testing.T) {
	c := newCreateTestController(t, nil)
	registerActiveSandbox(t, c, "sb-dns", &CubeNetworkConfig{}, []string{"169.254.0.53/32"})

	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-dns",
		CubeNetworkConfig: &CubeNetworkConfig{AllowOut: []string{"api.example.com"}},
	}); err != nil {
		t.Fatalf("UpdateNetworkPolicy: %v", err)
	}

	allow := c.cubevsAdapter.(*fakeCubeVSAdapter).updatedPolicies[0].opts.AllowOut
	if allow == nil {
		t.Fatal("CubeVS received no allow_out")
	}
	var sawResolver bool
	for _, target := range *allow {
		if target == "169.254.0.53/32" {
			sawResolver = true
		}
	}
	if !sawResolver {
		t.Errorf("resolver CIDR dropped by update: %v, DNS would break", *allow)
	}
}

// TestUpdateNetworkPolicyFallsBackToCallerResolvers covers the upgrade path: a
// sandbox created before the runtime recorded its resolvers has them installed
// but unidentifiable, so the caller's list is used instead of revoking DNS —
// and persisted, so the fallback is needed at most once per sandbox.
func TestUpdateNetworkPolicyFallsBackToCallerResolvers(t *testing.T) {
	c := newCreateTestController(t, nil)
	registerActiveSandbox(t, c, "sb-legacy", &CubeNetworkConfig{}, nil)

	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-legacy",
		CubeNetworkConfig: &CubeNetworkConfig{AllowOut: []string{"api.example.com"}},
		DNSAllowOutCIDRs:  []string{"169.254.0.53/32"},
	}); err != nil {
		t.Fatalf("UpdateNetworkPolicy: %v", err)
	}

	allow := *c.cubevsAdapter.(*fakeCubeVSAdapter).updatedPolicies[0].opts.AllowOut
	var sawResolver bool
	for _, target := range allow {
		if target == "169.254.0.53/32" {
			sawResolver = true
		}
	}
	if !sawResolver {
		t.Errorf("caller-supplied resolver was ignored for a legacy sandbox: %v", allow)
	}

	persisted, err := c.store.Load("sb-legacy", StateFileSuccess)
	if err != nil {
		t.Fatalf("reload success state: %v", err)
	}
	if len(persisted.DNSAllowOutCIDRs) != 1 || persisted.DNSAllowOutCIDRs[0] != "169.254.0.53/32" {
		t.Errorf("resolver list not backfilled into state: %v", persisted.DNSAllowOutCIDRs)
	}
}

// TestUpdateNetworkPolicyDropsResolversWithoutDomains is the other half of the
// gate: once no rule needs DNS, the implicit resolver exception goes away too.
// TestUpdateNetworkPolicyDropsResolversWithoutDomains checks the other half of
// the resolver gate: an IP-only policy must not inherit DNS access.
//
// The bare-literal cases are the ones that matter. A DNS name-shape check
// accepts "2.2.2.2" because digits are valid label characters, so gating on it
// silently folded the resolver into every IP-only policy. Masked forms like
// "2.2.2.2/32" happen to fail that check on the slash, which is why they cannot
// stand in for this.
func TestUpdateNetworkPolicyDropsResolversWithoutDomains(t *testing.T) {
	l7Port := 443
	for _, tc := range []struct {
		name string
		cfg  *CubeNetworkConfig
	}{
		{"bare IPv4", &CubeNetworkConfig{AllowOut: []string{"2.2.2.2"}}},
		{"masked IPv4", &CubeNetworkConfig{AllowOut: []string{"2.2.2.2/32"}}},
		{"subnet", &CubeNetworkConfig{AllowOut: []string{"203.0.113.0/24"}}},
		{"bare IPv4 L7 host", &CubeNetworkConfig{Rules: []*EgressRule{{
			Name: "ip-host",
			Match: &EgressRuleMatch{
				Host: stringPtr("2.2.2.2"), Port: &l7Port, Scheme: stringPtr("https"),
			},
			Action: &EgressRuleAction{Allow: true},
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCreateTestController(t, nil)
			registerActiveSandbox(t, c, "sb-nodns", &CubeNetworkConfig{}, []string{"169.254.0.53/32"})

			if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
				SandboxID:         "sb-nodns",
				CubeNetworkConfig: tc.cfg,
			}); err != nil {
				t.Fatalf("UpdateNetworkPolicy: %v", err)
			}

			// A rules-only policy leaves AllowOut unset, which is itself the
			// expected outcome here.
			allow := c.cubevsAdapter.(*fakeCubeVSAdapter).updatedPolicies[0].opts.AllowOut
			if allow == nil {
				return
			}
			for _, target := range *allow {
				if target == "169.254.0.53/32" {
					t.Errorf("resolver CIDR kept for an IP-only policy: %v", *allow)
				}
			}
		})
	}
}

// TestUpdateNetworkPolicyEmptyRulesDeletesEgressPolicy pins the empty-rule-set
// fix: PutPolicy short-circuits on no rules, so clearing every L7 rule has to
// go through DeletePolicy or the old rules keep intercepting.
func TestUpdateNetworkPolicyEmptyRulesDeletesEgressPolicy(t *testing.T) {
	c := newCreateTestController(t, nil)
	registerActiveSandbox(t, c, "sb-clear", &CubeNetworkConfig{
		Rules: []*EgressRule{{Name: "r1"}},
	}, nil)

	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-clear",
		CubeNetworkConfig: &CubeNetworkConfig{},
	}); err != nil {
		t.Fatalf("UpdateNetworkPolicy: %v", err)
	}

	egress := c.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if egress.deleteCalls != 1 {
		t.Errorf("DeletePolicy calls=%d, want 1", egress.deleteCalls)
	}
	if egress.putCalls != 0 {
		t.Errorf("PutPolicy calls=%d, want 0 for an empty rule set", egress.putCalls)
	}
}

// TestUpdateNetworkPolicyL7UntouchedSkipsCubeEgress pins that an L3-only update
// never talks to CubeEgress. Otherwise a sandbox that has no L7 rules — and so
// nothing installed on the proxy — would still fail its policy updates whenever
// CubeEgress happens to be down.
func TestUpdateNetworkPolicyL7UntouchedSkipsCubeEgress(t *testing.T) {
	c := newCreateTestController(t, nil)
	registerActiveSandbox(t, c, "sb-l3only", &CubeNetworkConfig{AllowOut: []string{"1.1.1.1/32"}}, nil)
	egress := c.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	egress.deleteErr = errors.New("connection refused")

	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-l3only",
		CubeNetworkConfig: &CubeNetworkConfig{AllowOut: []string{"2.2.2.2/32"}},
	}); err != nil {
		t.Fatalf("L3-only update failed because of CubeEgress: %v", err)
	}
	if egress.deleteCalls != 0 || egress.putCalls != 0 {
		t.Errorf("CubeEgress was contacted for an L3-only update: delete=%d put=%d",
			egress.deleteCalls, egress.putCalls)
	}
}

func TestUpdateNetworkPolicyUnknownSandbox(t *testing.T) {
	c := newCreateTestController(t, nil)
	err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-missing",
		CubeNetworkConfig: &CubeNetworkConfig{},
	})
	if !errors.Is(err, ErrNetworkNotActive) {
		t.Fatalf("err=%v, want ErrNetworkNotActive so callers can return a conflict", err)
	}
}

// TestUpdateNetworkPolicyKeepsOldPolicyOnCubeVSFailure pins the no-rollback
// contract's safe half: a failed update must not advance the durable state, so
// a restart re-applies the policy the sandbox actually ran with.
func TestUpdateNetworkPolicyKeepsOldPolicyOnCubeVSFailure(t *testing.T) {
	c := newCreateTestController(t, nil)
	registerActiveSandbox(t, c, "sb-fail", &CubeNetworkConfig{AllowOut: []string{"1.1.1.1/32"}}, nil)
	c.cubevsAdapter.(*fakeCubeVSAdapter).updateTAPPolicyErr = errors.New("map update failed")

	if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{
		SandboxID:         "sb-fail",
		CubeNetworkConfig: &CubeNetworkConfig{AllowOut: []string{"2.2.2.2/32"}},
	}); err == nil {
		t.Fatal("UpdateNetworkPolicy succeeded despite a CubeVS failure")
	}

	persisted, err := c.store.Load("sb-fail", StateFileSuccess)
	if err != nil {
		t.Fatalf("reload success state: %v", err)
	}
	if persisted.CubeNetworkConfig.AllowOut[0] != "1.1.1.1/32" {
		t.Errorf("failed update was persisted: %v", persisted.CubeNetworkConfig.AllowOut)
	}
}

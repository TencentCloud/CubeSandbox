// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/vishvananda/netlink"
)

// installEnsureMocks installs concurrency-safe no-op defaults for every package
// level hook touched by EnsureNetwork/ReleaseNetwork and restores the originals
// on cleanup. Individual tests override specific hooks after calling this.
func installEnsureMocks(t *testing.T) {
	t.Helper()
	oldNewTap := newTapFunc
	oldRestore := restoreTapFunc
	oldAddTap := cubevsAddTAPDevice
	oldDelTap := cubevsDelTAPDevice
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldAddPort := cubevsAddPortMap
	oldDelPort := cubevsDelPortMap
	oldList := listCubeTapsFunc
	oldDestroy := destroyTapFunc
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		newTapFunc = oldNewTap
		restoreTapFunc = oldRestore
		cubevsAddTAPDevice = oldAddTap
		cubevsDelTAPDevice = oldDelTap
		cubevsPrepareTAPPolicy = oldPrepareTap
		cubevsAddPortMap = oldAddPort
		cubevsDelPortMap = oldDelPort
		listCubeTapsFunc = oldList
		destroyTapFunc = oldDestroy
		addARPEntryFunc = oldARP
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{Name: tapName(ip.String()), Index: 1, IP: ip, File: newTestTapFile(t)}, nil
	}
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		if tap.File == nil {
			tap.File = newTestTapFile(t)
		}
		return tap, nil
	}
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsDelTAPDevice = func(uint32, net.IP) error { return nil }
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }
	cubevsAddPortMap = func(uint32, uint16, uint16) error { return nil }
	cubevsDelPortMap = func(uint32, uint16, uint16) error { return nil }
	listCubeTapsFunc = func() (map[string]*tapDevice, error) { return map[string]*tapDevice{}, nil }
	destroyTapFunc = func(int) error { return nil }
	addARPEntryFunc = func(net.IP, string, int) error { return nil }
	netlinkRouteListFiltered = func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, nil }
	netlinkRouteReplace = func(*netlink.Route) error { return nil }
}

func newConcurrencyTestService(t *testing.T) *localService {
	t.Helper()
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	return &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{min: 10000, max: 10999, next: 10000, assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMInnerIP: "169.254.68.6", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmGwDestIP: "169.254.68.5", MvmMask: 30, MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		quarantinedTaps:   make(map[string]*tapDevice),
		destroyFailedTaps: make(map[string]*tapDevice),
		creating:          make(map[string]chan struct{}),
	}
}

// TestEnsureNetworkConcurrentSameSandboxDeduplicates asserts that concurrent
// duplicate EnsureNetwork calls for the same sandbox collapse into a single
// creation (one tap, one IP) and that each caller receives an independent
// response clone.
func TestEnsureNetworkConcurrentSameSandboxDeduplicates(t *testing.T) {
	installEnsureMocks(t)
	var created int32
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		atomic.AddInt32(&created, 1)
		return &tapDevice{Name: tapName(ip.String()), Index: 100, IP: ip, File: newTestTapFile(t)}, nil
	}

	svc := newConcurrencyTestService(t)
	const n = 16
	resps := make([]*EnsureNetworkResponse, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resps[i], errs[i] = svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sb"})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("EnsureNetwork[%d] error=%v", i, errs[i])
		}
	}
	if got := atomic.LoadInt32(&created); got != 1 {
		t.Fatalf("newTap created=%d, want 1 (dedup failed)", got)
	}
	// reserved network/gateway/broadcast + one allocated IP.
	if svc.allocator.usedIPNum != 4 {
		t.Fatalf("usedIPNum=%d, want 4 (one IP allocated)", svc.allocator.usedIPNum)
	}
	for i := 1; i < n; i++ {
		if resps[i].PersistMetadata["sandbox_ip"] != resps[0].PersistMetadata["sandbox_ip"] {
			t.Fatalf("sandbox_ip mismatch: [0]=%s [%d]=%s", resps[0].PersistMetadata["sandbox_ip"], i, resps[i].PersistMetadata["sandbox_ip"])
		}
	}
	// Responses must be independent clones: mutating one must not affect another.
	resps[0].Interfaces[0].Name = "mutated"
	resps[0].PersistMetadata["sandbox_ip"] = "mutated"
	if resps[1].Interfaces[0].Name == "mutated" {
		t.Fatal("response Interfaces slice is shared between callers")
	}
	if resps[1].PersistMetadata["sandbox_ip"] == "mutated" {
		t.Fatal("response PersistMetadata map is shared between callers")
	}
}

// TestEnsureNetworkConcurrentDifferentSandboxesRunInParallel proves the global
// mutex no longer serializes unrelated sandboxes: all M creations must reach the
// (blocked) newTap step concurrently, which is impossible if a global lock were
// held across the heavy work.
func TestEnsureNetworkConcurrentDifferentSandboxesRunInParallel(t *testing.T) {
	installEnsureMocks(t)
	const m = 8
	var entered int32
	allIn := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		if atomic.AddInt32(&entered, 1) == int32(m) {
			once.Do(func() { close(allIn) })
		}
		<-release
		return &tapDevice{Name: tapName(ip.String()), Index: 100, IP: ip, File: newTestTapFile(t)}, nil
	}

	svc := newConcurrencyTestService(t)
	var wg sync.WaitGroup
	for i := 0; i < m; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: fmt.Sprintf("sb-%d", i)})
		}(i)
	}

	select {
	case <-allIn:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatalf("EnsureNetwork serialized: only %d/%d reached newTap concurrently", atomic.LoadInt32(&entered), m)
	}
	close(release)
	wg.Wait()
}

// TestReleaseNetworkWaitsForInflightCreationNoOrphan reproduces the create-in-
// flight vs Release race: Release must not observe "not found" and no-op while a
// tap is being built. It must wait for the creation to commit and then release
// it, leaving no orphaned tap.
func TestReleaseNetworkWaitsForInflightCreationNoOrphan(t *testing.T) {
	installEnsureMocks(t)
	var delTap int32
	cubevsDelTAPDevice = func(uint32, net.IP) error { atomic.AddInt32(&delTap, 1); return nil }
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{Name: tapName(ip.String()), Index: 101, IP: ip, File: newTestTapFile(t)}, nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		close(entered)
		<-release
		return nil
	}

	svc := newConcurrencyTestService(t)
	ensureErr := make(chan error, 1)
	go func() {
		_, err := svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sb"})
		ensureErr <- err
	}()

	<-entered // creation is in flight and registered in s.creating

	relDone := make(chan *ReleaseNetworkResponse, 1)
	go func() {
		resp, err := svc.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sb"})
		if err != nil {
			t.Errorf("ReleaseNetwork error=%v", err)
		}
		relDone <- resp
	}()

	// Release must block on the in-flight creation guard, not return a no-op.
	select {
	case <-relDone:
		close(release)
		t.Fatal("ReleaseNetwork returned before creation finished (orphan window)")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-ensureErr; err != nil {
		t.Fatalf("EnsureNetwork error=%v", err)
	}
	resp := <-relDone
	if resp == nil || !resp.Released {
		t.Fatalf("ReleaseNetwork resp=%+v, want Released=true", resp)
	}

	svc.mu.Lock()
	statesLen := len(svc.states)
	creatingLen := len(svc.creating)
	svc.mu.Unlock()
	if statesLen != 0 {
		t.Fatalf("states len=%d, want 0 (orphaned network left active)", statesLen)
	}
	if creatingLen != 0 {
		t.Fatalf("creating len=%d, want 0", creatingLen)
	}
	if atomic.LoadInt32(&delTap) == 0 {
		t.Fatal("cubevsDelTAPDevice not called: tap was not actually released")
	}
}

// TestCreateStateRollbackRecyclesPoolTap asserts that when a pooled tap is used
// and a later step fails, the tap is recycled back to the pool (its IP stays
// allocated) and host ports are released.
func TestCreateStateRollbackRecyclesPoolTap(t *testing.T) {
	installEnsureMocks(t)
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		return errors.New("register boom")
	}

	svc := newConcurrencyTestService(t)
	pooled := &tapDevice{Name: tapName("192.168.0.50"), Index: 70, IP: net.ParseIP("192.168.0.50").To4()}
	svc.allocator.Assign(pooled.IP)
	svc.mu.Lock()
	svc.enqueueTapLocked(pooled)
	svc.mu.Unlock()
	usedBefore := svc.allocator.usedIPNum

	_, err := svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID:    "sb",
		PortMappings: []PortMapping{{ContainerPort: 80}},
	})
	if err == nil {
		t.Fatal("EnsureNetwork: expected register failure")
	}

	svc.mu.Lock()
	poolLen := len(svc.tapPool)
	statesLen := len(svc.states)
	svc.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("tapPool len=%d, want 1 (pool tap recycled)", poolLen)
	}
	if statesLen != 0 {
		t.Fatalf("states len=%d, want 0", statesLen)
	}
	if svc.allocator.usedIPNum != usedBefore {
		t.Fatalf("usedIPNum=%d, want %d (pool tap IP must stay allocated)", svc.allocator.usedIPNum, usedBefore)
	}
	svc.ports.mu.Lock()
	assigned := len(svc.ports.assigned)
	svc.ports.mu.Unlock()
	if assigned != 0 {
		t.Fatalf("ports assigned=%d, want 0 (host port leaked)", assigned)
	}
}

func TestCreateStateRollbackWithholdsPoolTapWhenCleanupFails(t *testing.T) {
	installEnsureMocks(t)
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		return errors.New("register boom")
	}
	cubevsDelTAPDevice = func(uint32, net.IP) error {
		return errors.New("cleanup boom")
	}

	svc := newConcurrencyTestService(t)
	pooled := &tapDevice{Name: tapName("192.168.0.50"), Index: 70, IP: net.ParseIP("192.168.0.50").To4()}
	svc.allocator.Assign(pooled.IP)
	svc.mu.Lock()
	svc.enqueueTapLocked(pooled)
	svc.mu.Unlock()

	_, err := svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID:    "sb",
		PortMappings: []PortMapping{{ContainerPort: 80}},
	})
	if err == nil {
		t.Fatal("EnsureNetwork: expected register failure")
	}
	if !isTapCleanupError(err) {
		t.Fatalf("EnsureNetwork error=%v, want tap cleanup error", err)
	}

	svc.mu.Lock()
	poolLen := len(svc.tapPool)
	abnormalLen := len(svc.abnormalTapPool)
	statesLen := len(svc.states)
	svc.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("tapPool len=%d, want 0 (cleanup failed tap must not be reused)", poolLen)
	}
	if abnormalLen != 1 {
		t.Fatalf("abnormalTapPool len=%d, want 1", abnormalLen)
	}
	if statesLen != 0 {
		t.Fatalf("states len=%d, want 0", statesLen)
	}
}

func TestCreateStateRollbackQueuesPoolTapWhenPrepareFails(t *testing.T) {
	installEnsureMocks(t)
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		return errors.New("register boom")
	}
	cubevsPrepareTAPPolicy = func(uint32) error {
		return errors.New("prepare boom")
	}

	svc := newConcurrencyTestService(t)
	pooled := &tapDevice{Name: tapName("192.168.0.51"), Index: 71, IP: net.ParseIP("192.168.0.51").To4()}
	svc.allocator.Assign(pooled.IP)
	svc.mu.Lock()
	svc.enqueueTapLocked(pooled)
	svc.mu.Unlock()

	_, err := svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID:    "sb",
		PortMappings: []PortMapping{{ContainerPort: 80}},
	})
	if err == nil {
		t.Fatal("EnsureNetwork: expected register failure")
	}

	svc.mu.Lock()
	poolLen := len(svc.tapPool)
	pendingLen := len(svc.abnormalTapPool)
	pendingStage := ""
	if pendingLen > 0 {
		pendingStage = svc.abnormalTapPool[0].LastStage
	}
	svc.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("tapPool len=%d, want 0 before pool preparation succeeds", poolLen)
	}
	if pendingLen != 1 {
		t.Fatalf("abnormalTapPool len=%d, want 1 pending preparation", pendingLen)
	}
	if pendingStage != abnormalStagePreparePool {
		t.Fatalf("pending stage=%q, want %q", pendingStage, abnormalStagePreparePool)
	}
}

// TestCreateStateRollbackDestroysNonPoolTapOnSaveFailure asserts that a freshly
// created (non-pool) tap is destroyed, its IP released, and cubevs metadata
// cleaned up when state persistence fails.
func TestCreateStateRollbackDestroysNonPoolTapOnSaveFailure(t *testing.T) {
	installEnsureMocks(t)
	var destroyCalls, delTapCalls int32
	destroyTapFunc = func(int) error { atomic.AddInt32(&destroyCalls, 1); return nil }
	cubevsDelTAPDevice = func(uint32, net.IP) error { atomic.AddInt32(&delTapCalls, 1); return nil }
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{Name: tapName(ip.String()), Index: 71, IP: ip, File: newTestTapFile(t)}, nil
	}

	dir := t.TempDir()
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	svc := newConcurrencyTestService(t)
	svc.store = store
	// Force store.Save to fail by occupying the target path with a directory.
	if err := os.Mkdir(filepath.Join(dir, "sb.json"), 0o755); err != nil {
		t.Fatalf("Mkdir error=%v", err)
	}
	usedBefore := svc.allocator.usedIPNum

	_, err = svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sb"})
	if err == nil {
		t.Fatal("EnsureNetwork: expected save failure")
	}
	if atomic.LoadInt32(&destroyCalls) == 0 {
		t.Fatal("non-pool tap was not destroyed on save failure")
	}
	if atomic.LoadInt32(&delTapCalls) == 0 {
		t.Fatal("cubevsDelTAPDevice not called on save failure (cubevs metadata leaked)")
	}
	if svc.allocator.usedIPNum != usedBefore {
		t.Fatalf("usedIPNum=%d, want %d (non-pool tap IP must be released)", svc.allocator.usedIPNum, usedBefore)
	}
	svc.mu.Lock()
	statesLen := len(svc.states)
	poolLen := len(svc.tapPool)
	svc.mu.Unlock()
	if statesLen != 0 || poolLen != 0 {
		t.Fatalf("residue after rollback: states=%d pool=%d", statesLen, poolLen)
	}
}

func TestReleaseNetworkDoesNotRecycleTapWhenCubeVSCleanupFails(t *testing.T) {
	installEnsureMocks(t)
	cubevsDelTAPDevice = func(uint32, net.IP) error {
		return errors.New("cleanup boom")
	}

	svc := newConcurrencyTestService(t)
	ip := net.ParseIP("192.168.0.60").To4()
	tap := &tapDevice{Name: tapName(ip.String()), Index: 80, IP: ip, File: newTestTapFile(t)}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:     "sb",
			NetworkHandle: "sb",
			TapName:       tap.Name,
			TapIfIndex:    tap.Index,
			SandboxIP:     ip.String(),
		},
		tap: tap,
	}
	svc.states["sb"] = state
	if err := svc.store.Save(&state.persistedState); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	_, err := svc.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sb", NetworkHandle: "sb"})
	if err == nil {
		t.Fatal("ReleaseNetwork: expected cleanup failure")
	}

	svc.mu.Lock()
	_, stillActive := svc.states["sb"]
	poolLen := len(svc.tapPool)
	svc.mu.Unlock()
	if !stillActive {
		t.Fatal("state was removed despite cleanup failure")
	}
	if poolLen != 0 {
		t.Fatalf("tapPool len=%d, want 0 (cleanup failed tap must not be reused)", poolLen)
	}
}

func TestReleaseNetworkQueuesTapWhenPoolPrepareFails(t *testing.T) {
	installEnsureMocks(t)
	cubevsPrepareTAPPolicy = func(uint32) error {
		return errors.New("prepare boom")
	}

	svc := newConcurrencyTestService(t)
	ip := net.ParseIP("192.168.0.61").To4()
	tap := &tapDevice{Name: tapName(ip.String()), Index: 81, IP: ip, File: newTestTapFile(t)}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:     "sb",
			NetworkHandle: "sb",
			TapName:       tap.Name,
			TapIfIndex:    tap.Index,
			SandboxIP:     ip.String(),
		},
		tap: tap,
	}
	svc.states["sb"] = state
	if err := svc.store.Save(&state.persistedState); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	resp, err := svc.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sb", NetworkHandle: "sb"})
	if err != nil {
		t.Fatalf("ReleaseNetwork error=%v", err)
	}
	if resp == nil || !resp.Released {
		t.Fatalf("ReleaseNetwork resp=%+v, want Released=true", resp)
	}

	svc.mu.Lock()
	_, stillActive := svc.states["sb"]
	poolLen := len(svc.tapPool)
	pendingLen := len(svc.abnormalTapPool)
	svc.mu.Unlock()
	if stillActive {
		t.Fatal("state remained active after successful cleanup")
	}
	if poolLen != 0 {
		t.Fatalf("tapPool len=%d, want 0 before pool preparation succeeds", poolLen)
	}
	if pendingLen != 1 {
		t.Fatalf("abnormalTapPool len=%d, want 1 pending pool preparation", pendingLen)
	}
}

// TestConcurrentEnsureReleaseStress exercises EnsureNetwork/ReleaseNetwork and
// the maintenance loop concurrently to surface data races on the shared pools
// and states map under the race detector.
func TestConcurrentEnsureReleaseStress(t *testing.T) {
	installEnsureMocks(t)
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		return &tapDevice{Name: tapName(ip.String()), Index: 1, IP: ip, File: newTestTapFile(t)}, nil
	}

	svc := newConcurrencyTestService(t)
	maintDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-maintDone:
				return
			default:
				svc.handleAbnormalTaps()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sb-%d", i%8)
			if _, err := svc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: id}); err == nil {
				_, _ = svc.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: id})
			}
		}(i)
	}
	wg.Wait()
	close(maintDone)
}

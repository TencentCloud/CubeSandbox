// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"net"
	"os"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
)

func TestTapPoolOnlyReadyCanBeAllocated(t *testing.T) {
	ready := mustReadyEntry(t, "tap-ready", 1, "10.0.0.2")
	cleaning := mustReadyEntry(t, "tap-cleaning", 2, "10.0.0.3")
	if err := cleaning.MarkActive("cleanup-owner"); err != nil {
		t.Fatal(err)
	}
	if err := cleaning.BeginCleanup("cleanup-owner"); err != nil {
		t.Fatal(err)
	}
	pool, err := NewTapPool(cleaning, ready)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := pool.Acquire("sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	if entry != ready || entry.State != TapPoolReady || entry.OwnerSandboxID != "sandbox1" {
		t.Fatalf("entry = %#v", entry)
	}
	if err := pool.CommitActive(entry, "sandbox1"); err != nil {
		t.Fatal(err)
	}
	if entry.State != TapPoolActive {
		t.Fatalf("state = %s", entry.State)
	}

	if _, err := pool.Acquire("sandbox2"); err == nil {
		t.Fatal("Acquire succeeded with no Ready entries")
	}
}

func TestTapPoolAcquireIsIdempotentForActiveOwner(t *testing.T) {
	entry := mustReadyEntry(t, "tap-ready", 1, "10.0.0.2")
	pool, err := NewTapPool(entry)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.Acquire("sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Acquire("sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent acquire returned different entries")
	}
}

func TestTapPoolCleanupThenReady(t *testing.T) {
	entry := mustReadyEntry(t, "tap-ready", 1, "10.0.0.2")
	pool, err := NewTapPool(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Acquire("sandbox1"); err != nil {
		t.Fatal(err)
	}
	cleaning, err := pool.BeginCleanup("sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	if cleaning.State != TapPoolCleaning {
		t.Fatalf("state = %s", cleaning.State)
	}
	if _, err := pool.Acquire("sandbox2"); err == nil {
		t.Fatal("Acquire succeeded while entry is Cleaning")
	}
	if err := pool.MarkReady(cleaning); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Acquire("sandbox2"); err != nil {
		t.Fatal(err)
	}
}

func TestTapPoolAddReservedPublishesNoAssignableWindow(t *testing.T) {
	pool, err := NewTapPool()
	if err != nil {
		t.Fatal(err)
	}
	entry := mustReadyEntry(t, "tap-new", 3, "10.0.0.4")
	if err := pool.AddReserved(entry, "sandbox-owner"); err != nil {
		t.Fatal(err)
	}
	if entry.State != TapPoolReady || entry.OwnerSandboxID != "sandbox-owner" {
		t.Fatalf("reserved entry = %#v", entry)
	}
	if _, err := pool.Acquire("other-sandbox"); err == nil {
		t.Fatal("another owner acquired atomically-reserved entry")
	}
	if got, err := pool.Acquire("sandbox-owner"); err != nil || got != entry {
		t.Fatalf("reserved owner acquire got=%#v err=%v", got, err)
	}
}

func TestTapPoolAddOwnerConflictDoesNotMutatePool(t *testing.T) {
	first, err := NewTapPoolEntry("tap-first", 1, net.ParseIP("10.0.0.2"), "sandbox1", TapPoolActive)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewTapPool(first)
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := NewTapPoolEntry("tap-conflict", 2, net.ParseIP("10.0.0.3"), "sandbox1", TapPoolActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(conflict); err == nil {
		t.Fatal("Add owner conflict succeeded, want error")
	}
	if _, ok := pool.GetByName("tap-conflict"); ok {
		t.Fatal("conflicting tap was inserted into byName")
	}
	if got := len(pool.Entries()); got != 1 {
		t.Fatalf("entries length=%d, want 1", got)
	}
	if entry, err := pool.Acquire("sandbox1"); err != nil || entry != first {
		t.Fatalf("existing owner entry changed: entry=%#v err=%v", entry, err)
	}
}

func TestTapPoolRejectsInvalidTransitions(t *testing.T) {
	// Guard the state machine against shortcuts that would bypass create/release
	// transaction boundaries in NetworkController.
	entry := mustReadyEntry(t, "tap-ready", 1, "10.0.0.2")
	if err := entry.BeginCleanup(""); err == nil {
		t.Fatal("Ready -> Cleaning via BeginCleanup succeeded, want error")
	}
	if err := entry.MarkReady(); err == nil {
		t.Fatal("Ready -> Ready via MarkReady succeeded, want error")
	}
	if err := entry.MarkActive("sandbox1"); err != nil {
		t.Fatal(err)
	}
	if err := entry.MarkActive("sandbox2"); err == nil {
		t.Fatal("Active -> Active with new owner succeeded, want error")
	}
}

func TestMarkTapReadyPublishesCleanedTap(t *testing.T) {
	controller, _ := newTapInventoryTestController(t, 0)
	entry, err := NewTapPoolEntry("tap-clean", 7, net.ParseIP("10.0.0.7"), "sandbox-old", TapPoolCleaning)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}
	tap := &tapDevice{
		Name:  entry.TapName,
		Index: entry.TapIfIndex,
		IP:    append(net.IP(nil), entry.SandboxIP...),
	}
	if err := controller.markTapReady(tap); err != nil {
		t.Fatal(err)
	}
	acquired, err := controller.tapPool.Acquire("sandbox-new")
	if err != nil {
		t.Fatal(err)
	}
	if acquired != entry {
		t.Fatalf("acquired entry=%#v, want %#v", acquired, entry)
	}
}

func TestReleaseAcquiredTapResetsSharedTapBeforeAssignable(t *testing.T) {
	controller, adapter := newTapInventoryTestController(t, 0)
	entry := mustReadyEntry(t, "tap-reserved", 8, "10.0.0.8")
	if err := controller.tapPool.Add(entry); err != nil {
		t.Fatal(err)
	}
	reserved, err := controller.tapPool.Acquire("sandbox-old")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "tap-reserved-*")
	if err != nil {
		t.Fatal(err)
	}
	tap := &tapDevice{
		Name:         reserved.TapName,
		Index:        reserved.TapIfIndex,
		IP:           append(net.IP(nil), reserved.SandboxIP...),
		File:         file,
		InUse:        true,
		LastError:    "old error",
		LastStage:    "old stage",
		PortMappings: []PortMapping{{HostPort: 20081, ContainerPort: 8081}},
	}
	adapter.closeHook = func(*os.File) {
		if _, err := controller.tapPool.Acquire("sandbox-new"); err == nil {
			t.Fatal("tap reservation was released before runtime fields were reset")
		}
		if tap.PortMappings != nil || tap.InUse || tap.LastError != "" || tap.LastStage != "" {
			t.Fatalf("tap runtime fields not reset before reservation release: %#v", tap)
		}
	}

	controller.releaseAcquiredTap("sandbox-old", tap, reserved)
	if tap.File != nil {
		t.Fatalf("tap file still cached after reservation release: %#v", tap.File)
	}
	acquired, err := controller.tapPool.Acquire("sandbox-new")
	if err != nil {
		t.Fatal(err)
	}
	if acquired != reserved {
		t.Fatalf("acquired entry=%#v, want %#v", acquired, reserved)
	}
}

func mustReadyEntry(t *testing.T, name string, ifindex int, ip string) *TapPoolEntry {
	t.Helper()
	entry, err := NewReadyTapPoolEntry(name, ifindex, net.ParseIP(ip))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// fakeTapDeviceAdapter is a non-privileged adapter used by pool warmup tests.
type fakeTapDeviceAdapter struct {
	createCount      int
	restoreCount     int
	openCount        int
	openErr          error
	closeHook        func(*os.File)
	restoreWithoutFD bool
	listResult       map[string]*tapDevice
	listErr          error
}

func (f *fakeTapDeviceAdapter) Create(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
	f.createCount++
	return &tapDevice{Name: tapName(ip.String()), Index: f.createCount, IP: append(net.IP(nil), ip.To4()...)}, nil
}

func (f *fakeTapDeviceAdapter) Restore(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
	f.restoreCount++
	if tap != nil && tap.File == nil && !f.restoreWithoutFD {
		file, err := os.CreateTemp("", "cube-test-tap-*")
		if err != nil {
			return nil, err
		}
		tap.File = file
	}
	return tap, nil
}

func (f *fakeTapDeviceAdapter) Open(_ string) (*os.File, error) {
	f.openCount++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return os.CreateTemp("", "cube-test-tap-*")
}

func (f *fakeTapDeviceAdapter) Close(file *os.File) {
	if f.closeHook != nil {
		f.closeHook(file)
	}
}

func (f *fakeTapDeviceAdapter) List() (map[string]*tapDevice, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeTapDeviceAdapter) GetByName(_ string) (*tapDevice, error) {
	return nil, nil
}

func (f *fakeTapDeviceAdapter) Destroy(_ int) error {
	return nil
}

func TestEnsureTapInventoryUsesTapDeviceAdapter(t *testing.T) {
	controller, adapter := newTapInventoryTestController(t, 2)

	if err := controller.ensureTapInventory(); err != nil {
		t.Fatal(err)
	}
	if adapter.createCount != 2 {
		t.Fatalf("createCount = %d, want 2", adapter.createCount)
	}
	if got := len(controller.tapPool.Entries()); got != 2 {
		t.Fatalf("tap pool entries = %d, want 2", got)
	}
}

func TestEnsureTapInventoryUsesTotalEntryCount(t *testing.T) {
	controller, adapter := newTapInventoryTestController(t, 3)
	ready := mustReadyEntry(t, "tap-ready", 1, "10.0.0.2")
	active, err := NewTapPoolEntry("tap-active", 2, net.ParseIP("10.0.0.3"), "sandbox-active", TapPoolActive)
	if err != nil {
		t.Fatal(err)
	}
	cleaning, err := NewTapPoolEntry("tap-cleaning", 3, net.ParseIP("10.0.0.4"), "sandbox-cleaning", TapPoolCleaning)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []*TapPoolEntry{ready, active, cleaning} {
		if err := controller.tapPool.Add(entry); err != nil {
			t.Fatal(err)
		}
	}

	if err := controller.ensureTapInventory(); err != nil {
		t.Fatal(err)
	}
	if adapter.createCount != 0 {
		t.Fatalf("createCount = %d, want 0 because total entries already meet target", adapter.createCount)
	}
}

func newTapInventoryTestController(t *testing.T, target int) (*NetworkController, *fakeTapDeviceAdapter) {
	t.Helper()
	pool, err := NewTapPool()
	if err != nil {
		t.Fatal(err)
	}
	allocator, err := newIPAllocator("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	ports, err := newPortBinder()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeTapDeviceAdapter{listResult: map[string]*tapDevice{}}
	controller := &NetworkController{
		cfg: Config{
			TapInitNum:   target,
			CIDR:         "10.0.0.0/29",
			MVMMacAddr:   "02:00:00:00:00:01",
			MvmMtu:       1500,
			MvmGwMacAddr: "02:00:00:00:00:02",
		},
		allocator:     allocator,
		ports:         ports,
		tapPool:       pool,
		tapAdapter:    adapter,
		cubevsAdapter: &fakeCubeVSAdapter{},
		cubeDev:       &systemnet.CubeDev{Index: 100},
	}
	return controller, adapter
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"net"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/vishvananda/netlink"
)

// ---------- reconcileState CIDR guard ----------

func TestReconcileStateRejectsOutOfCIDRSandbox(t *testing.T) {
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: mustParseCIDR(t, "192.168.100.0/24"), LinkIndex: 16}}, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		allocator: allocator,
		ports:     &portAllocator{},
		cfg:       Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:   &cubeDev{Index: 16},
		states:    make(map[string]*managedState),
	}

	// SandboxIP from old CIDR 192.168.0.0/18 → not in new 192.168.100.0/24
	state := &managedState{
		persistedState: persistedState{
			SandboxID:  "sandbox-old-cidr",
			TapName:    "z192.168.0.100",
			TapIfIndex: 100,
			SandboxIP:  "192.168.0.100",
		},
	}

	err = svc.reconcileState(t.Context(), state)
	if err == nil {
		t.Fatal("reconcileState should return error for out-of-CIDR sandbox IP, got nil")
	}
}

func TestReconcileStateAcceptsInCIDRSandbox(t *testing.T) {
	oldRestore := restoreTapFunc
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAddTap := cubevsAddTAPDevice
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		restoreTapFunc = oldRestore
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAddTap
		addARPEntryFunc = oldARP
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		tap.File = newTestTapFile(t)
		return tap, nil
	}
	cubevsAttachFilter = func(uint32) error { return nil }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) { return &cubevs.TAPDevice{}, nil }
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	addARPEntryFunc = func(net.IP, string, int) error { return nil }
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: mustParseCIDR(t, "192.168.100.0/24"), LinkIndex: 16}}, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		allocator: allocator,
		ports:     &portAllocator{},
		cfg:       Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:   &cubeDev{Index: 16},
		states:    make(map[string]*managedState),
	}

	state := &managedState{
		persistedState: persistedState{
			SandboxID:  "sandbox-new-cidr",
			TapName:    "z192.168.100.5",
			TapIfIndex: 105,
			SandboxIP:  "192.168.100.5",
		},
	}

	err = svc.reconcileState(t.Context(), state)
	if err != nil {
		t.Fatalf("reconcileState should accept in-CIDR sandbox IP, got error=%v", err)
	}
}

func TestReconcileStateRejectsInvalidSandboxIP(t *testing.T) {
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: mustParseCIDR(t, "192.168.100.0/24"), LinkIndex: 16}}, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		allocator: allocator,
		ports:     &portAllocator{},
		cfg:       Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:   &cubeDev{Index: 16},
		states:    make(map[string]*managedState),
	}

	tests := []struct {
		name      string
		sandboxIP string
	}{
		{"empty-ip", ""},
		{"invalid-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &managedState{
				persistedState: persistedState{
					SandboxID: "sandbox-" + tt.name,
					SandboxIP: tt.sandboxIP,
				},
			}
			err := svc.reconcileState(t.Context(), state)
			if err == nil {
				t.Fatal("reconcileState should return error for invalid sandbox IP")
			}
		})
	}
}

// ---------- recover with CIDR change: out-of-range taps destroyed ----------

func TestRecoverDestroysOutOfRangeTapsOnCIDRChange(t *testing.T) {
	oldList := listCubeTapsFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldDelTap := cubevsDelTAPDevice
	oldDestroyTap := destroyTapFunc
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsDelTAPDevice = oldDelTap
		destroyTapFunc = oldDestroyTap
	})

	// Simulate taps from the OLD CIDR 192.168.0.0/18
	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.0.100": {
				Name:  "z192.168.0.100",
				Index: 100,
				IP:    net.ParseIP("192.168.0.100").To4(),
			},
			"192.168.0.101": {
				Name:  "z192.168.0.101",
				Index: 101,
				IP:    net.ParseIP("192.168.0.101").To4(),
			},
		}, nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) {
		return map[uint16]cubevs.MVMPort{}, nil
	}

	delTapCalls := 0
	cubevsDelTAPDevice = func(ifindex uint32, ip net.IP) error {
		delTapCalls++
		return nil
	}

	destroyedIfindexes := []int{}
	destroyTapFunc = func(ifIdx int) error {
		destroyedIfindexes = append(destroyedIfindexes, ifIdx)
		return nil
	}

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}

	// NEW CIDR is 192.168.100.0/24 — the old taps are NOT in this range
	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}

	// Both old taps should have been destroyed
	if len(destroyedIfindexes) != 2 {
		t.Fatalf("destroyedIfindexes=%v, want 2 destroyed taps", destroyedIfindexes)
	}
	if delTapCalls != 2 {
		t.Fatalf("delTapCalls=%d, want 2", delTapCalls)
	}

	// No taps in the pool (they were destroyed, not pooled)
	if len(svc.tapPool) != 0 {
		t.Fatalf("tapPool len=%d, want 0 (out-of-range taps should not be pooled)", len(svc.tapPool))
	}

	// No active states
	if len(svc.states) != 0 {
		t.Fatalf("states len=%d, want 0", len(svc.states))
	}
}

// ---------- recover: persisted state with out-of-CIDR IP skipped gracefully ----------

func TestRecoverSkipsPersistedStateWithOutOfCIDRIP(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldDelTap := cubevsDelTAPDevice
	oldDelPort := cubevsDelPortMap
	oldDestroyTap := destroyTapFunc
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsDelTAPDevice = oldDelTap
		cubevsDelPortMap = oldDelPort
		destroyTapFunc = oldDestroyTap
	})

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}

	// A persisted state from the OLD CIDR (192.168.0.0/18)
	persisted := &persistedState{
		SandboxID:     "sandbox-old-1",
		NetworkHandle: "sandbox-old-1",
		TapName:       "z192.168.0.50",
		TapIfIndex:    50,
		SandboxIP:     "192.168.0.50",
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	// The kernel has a matching tap (it hasn't been cleaned up yet)
	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.0.50": {
				Name:  "z192.168.0.50",
				Index: 50,
				IP:    net.ParseIP("192.168.0.50").To4(),
			},
		}, nil
	}

	// Because the tap IP is outside the new CIDR, it will be destroyed
	// during recover and never reach reconcileState. So restoreTapFunc
	// should NOT be called.
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		t.Fatal("restoreTapFunc should not be called for out-of-range tap")
		return nil, nil
	}

	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) {
		return map[uint16]cubevs.MVMPort{}, nil
	}

	delTapCalls := 0
	cubevsDelTAPDevice = func(uint32, net.IP) error {
		delTapCalls++
		return nil
	}
	cubevsDelPortMap = func(uint32, uint16, uint16) error { return nil }

	destroyTapCalls := 0
	destroyTapFunc = func(ifIdx int) error {
		destroyTapCalls++
		return nil
	}

	// NEW CIDR — 192.168.100.0/24
	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}

	// The old tap should have been destroyed
	if destroyTapCalls != 1 {
		t.Fatalf("destroyTapCalls=%d, want 1", destroyTapCalls)
	}
	if delTapCalls != 1 {
		t.Fatalf("delTapCalls=%d, want 1", delTapCalls)
	}

	// The sandbox should not be in active states
	if _, ok := svc.states["sandbox-old-1"]; ok {
		t.Fatal("sandbox-old-1 should not be in active states after recover with new CIDR")
	}

	// The persisted state file should have been cleaned up
	// (the stale state cleanup in recover deletes states not recovered)
}

// ---------- recover: reconcileState failure skips sandbox and cleans up ----------

func TestRecoverSkipsSandboxWhenReconcileFailsForCIDRChange(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAddTap := cubevsAddTAPDevice
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldDelTap := cubevsDelTAPDevice
	oldDelPort := cubevsDelPortMap
	oldDestroyTap := destroyTapFunc
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAddTap
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsDelTAPDevice = oldDelTap
		cubevsDelPortMap = oldDelPort
		destroyTapFunc = oldDestroyTap
		addARPEntryFunc = oldARP
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}

	// Persisted state with an IP that IS in the new CIDR but the tap device
	// is missing on the host (simulate a partial state).
	persisted := &persistedState{
		SandboxID:     "sandbox-partial",
		NetworkHandle: "sandbox-partial",
		TapName:       "z192.168.100.10",
		TapIfIndex:    110,
		SandboxIP:     "192.168.100.10",
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	// The kernel tap exists and is in-range
	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.100.10": {
				Name:  "z192.168.100.10",
				Index: 110,
				IP:    net.ParseIP("192.168.100.10").To4(),
			},
		}, nil
	}

	// restoreTap succeeds
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		tap.File = newTestTapFile(t)
		return tap, nil
	}

	// reconcileState will fail because cubevsAttachFilter fails
	attachErr := "attach-filter-failed"
	cubevsAttachFilter = func(uint32) error { return fmt.Errorf("%s", attachErr) }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) { return &cubevs.TAPDevice{}, nil }
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) {
		return map[uint16]cubevs.MVMPort{}, nil
	}

	delTapCalls := 0
	cubevsDelTAPDevice = func(uint32, net.IP) error {
		delTapCalls++
		return nil
	}
	cubevsDelPortMap = func(uint32, uint16, uint16) error { return nil }

	destroyTapCalls := 0
	destroyTapFunc = func(ifIdx int) error {
		destroyTapCalls++
		return nil
	}

	addARPEntryFunc = func(net.IP, string, int) error { return nil }
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: mustParseCIDR(t, "192.168.100.0/24"), LinkIndex: 16}}, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	// recover should NOT fail even though reconcileState fails
	if err := svc.recover(); err != nil {
		t.Fatalf("recover should not fail when reconcileState fails, got error=%v", err)
	}

	// The sandbox should be skipped (not in active states)
	if _, ok := svc.states["sandbox-partial"]; ok {
		t.Fatal("sandbox-partial should be skipped when reconcileState fails")
	}

	// The tap should have been cleaned up
	if destroyTapCalls != 1 {
		t.Fatalf("destroyTapCalls=%d, want 1", destroyTapCalls)
	}

	// The IP should have been released back to the allocator.
	// After recover, usedIPNum should equal the base count (network+gw+broadcast = 3),
	// meaning the previously assigned 192.168.100.10 was released.
	allocator.Lock()
	usedAfter := allocator.usedIPNum
	allocator.Unlock()
	if usedAfter != 3 {
		t.Fatalf("allocator usedIPNum=%d after recover, want 3 (net+gw+bcast only); IP was not released", usedAfter)
	}
}

// ---------- dequeueTapLocked: out-of-range tap from pool is destroyed ----------

func TestDequeueTapLockedDestroysOutOfRangeTapAfterCIDRChange(t *testing.T) {
	oldDelTap := cubevsDelTAPDevice
	oldDestroyTap := destroyTapFunc
	t.Cleanup(func() {
		cubevsDelTAPDevice = oldDelTap
		destroyTapFunc = oldDestroyTap
	})

	delTapCalls := 0
	cubevsDelTAPDevice = func(uint32, net.IP) error {
		delTapCalls++
		return nil
	}

	destroyedIfindexes := []int{}
	destroyTapFunc = func(ifIdx int) error {
		destroyedIfindexes = append(destroyedIfindexes, ifIdx)
		return nil
	}

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		allocator: allocator,
		ports:     &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:       Config{CIDR: "192.168.100.0/24", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:   &cubeDev{Index: 16},
		states:    make(map[string]*managedState),
	}

	// Manually inject an old-CIDR tap into the pool
	svc.tapPool = []*tapDevice{
		{
			Name:  "z192.168.0.50",
			Index: 50,
			IP:    net.ParseIP("192.168.0.50").To4(),
		},
	}

	tap := svc.dequeueTapLocked()
	if tap != nil {
		t.Fatalf("dequeueTapLocked should return nil for out-of-range tap, got %+v", tap)
	}
	if len(destroyedIfindexes) != 1 || destroyedIfindexes[0] != 50 {
		t.Fatalf("destroyedIfindexes=%v, want [50]", destroyedIfindexes)
	}
	if delTapCalls != 1 {
		t.Fatalf("delTapCalls=%d, want 1", delTapCalls)
	}
	if len(svc.tapPool) != 0 {
		t.Fatalf("tapPool len=%d, want 0 after destroying out-of-range tap", len(svc.tapPool))
	}
}

// ---------- ensureTapInventory after CIDR change ----------

func TestEnsureNetworkAllocatesFromNewCIDRAfterChange(t *testing.T) {
	oldNewTap := newTapFunc
	oldRestore := restoreTapFunc
	oldAddTap := cubevsAddTAPDevice
	oldDelTap := cubevsDelTAPDevice
	oldAddPort := cubevsAddPortMap
	oldDelPort := cubevsDelPortMap
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		newTapFunc = oldNewTap
		restoreTapFunc = oldRestore
		cubevsAddTAPDevice = oldAddTap
		cubevsDelTAPDevice = oldDelTap
		cubevsAddPortMap = oldAddPort
		cubevsDelPortMap = oldDelPort
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	var allocatedIP net.IP
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		allocatedIP = ip
		return &tapDevice{
			Name:  tapName(ip.String()),
			Index: 200,
			IP:    ip,
			File:  newTestTapFile(t),
		}, nil
	}
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		if tap.File == nil {
			tap.File = newTestTapFile(t)
		}
		return tap, nil
	}
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		return nil
	}
	cubevsDelTAPDevice = func(uint32, net.IP) error { return nil }
	cubevsAddPortMap = func(uint32, uint16, uint16) error { return nil }
	cubevsDelPortMap = func(uint32, uint16, uint16) error { return nil }
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return nil, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}

	allocator, err := newIPAllocator("192.168.100.0/24")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.100.0/24", MVMInnerIP: "169.254.68.6", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmGwDestIP: "169.254.68.5", MvmMask: 30, MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	resp, err := svc.EnsureNetwork(t.Context(), &EnsureNetworkRequest{SandboxID: "sandbox-new-cidr"})
	if err != nil {
		t.Fatalf("EnsureNetwork error=%v", err)
	}

	// The sandbox IP must be in the new CIDR 192.168.100.0/24
	sandboxIP := resp.PersistMetadata["sandbox_ip"]
	if sandboxIP == "" {
		t.Fatal("sandbox_ip missing from PersistMetadata")
	}
	ip := net.ParseIP(sandboxIP).To4()
	if ip == nil {
		t.Fatalf("sandbox_ip %q is not a valid IPv4", sandboxIP)
	}
	if !allocator.Contains(ip) {
		t.Fatalf("sandbox_ip %s is NOT in new CIDR 192.168.100.0/24", sandboxIP)
	}

	// Verify the allocated IP from newTapFunc matches
	if allocatedIP == nil || !allocatedIP.Equal(ip) {
		t.Fatalf("allocatedIP=%v, sandboxIP=%v, want equal", allocatedIP, ip)
	}
}


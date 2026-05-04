// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"errors"
	"net"
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/vishvananda/netlink"
)

func TestCubeVSTapRegistration(t *testing.T) {
	opts := cubeVSTapRegistration(&CubeVSContext{
		AllowInternetAccess: boolPtr(true),
		AllowOut:            []string{"10.0.0.0/8"},
		DenyOut:             []string{"192.168.0.0/16"},
	})
	if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != true {
		t.Fatalf("opts.AllowInternetAccess=%v, want true", opts.AllowInternetAccess)
	}
	if opts.AllowOut == nil || len(*opts.AllowOut) != 1 || (*opts.AllowOut)[0] != "10.0.0.0/8" {
		t.Fatalf("opts.AllowOut=%v, want [10.0.0.0/8]", opts.AllowOut)
	}
	if opts.DenyOut == nil || len(*opts.DenyOut) != 1 || (*opts.DenyOut)[0] != "192.168.0.0/16" {
		t.Fatalf("opts.DenyOut=%v, want [192.168.0.0/16]", opts.DenyOut)
	}
}

func TestCubeVSTapRegistrationBlockAll(t *testing.T) {
	opts := cubeVSTapRegistration(&CubeVSContext{
		AllowInternetAccess: boolPtr(false),
	})
	if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != false {
		t.Fatalf("opts.AllowInternetAccess=%v, want false", opts.AllowInternetAccess)
	}
}

func TestRefreshCubeVSTapReattachesFilter(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldAdd := cubevsAddTAPDevice
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsAddTAPDevice = oldAdd
	})

	attachCalls := 0
	addCalls := 0
	cubevsAttachFilter = func(ifindex uint32) error {
		attachCalls++
		if ifindex != 17 {
			t.Fatalf("AttachFilter ifindex=%d, want 17", ifindex)
		}
		return nil
	}
	cubevsGetTAPDevice = func(ifindex uint32) (*cubevs.TAPDevice, error) {
		if ifindex != 17 {
			t.Fatalf("GetTAPDevice ifindex=%d, want 17", ifindex)
		}
		return &cubevs.TAPDevice{Ifindex: int(ifindex)}, nil
	}
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		addCalls++
		return nil
	}

	svc := &localService{}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:  "sandbox-1",
			TapName:    "z192.168.0.2",
			TapIfIndex: 17,
			SandboxIP:  "192.168.0.2",
		},
	}

	if err := svc.refreshCubeVSTap(state); err != nil {
		t.Fatalf("refreshCubeVSTap error=%v", err)
	}
	if attachCalls != 1 {
		t.Fatalf("AttachFilter calls=%d, want 1", attachCalls)
	}
	if addCalls != 0 {
		t.Fatalf("AddTAPDevice calls=%d, want 0", addCalls)
	}
}

func TestRefreshCubeVSTapReRegistersMissingMapEntry(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldAdd := cubevsAddTAPDevice
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsAddTAPDevice = oldAdd
	})

	cubevsAttachFilter = func(ifindex uint32) error {
		if ifindex != 23 {
			t.Fatalf("AttachFilter ifindex=%d, want 23", ifindex)
		}
		return nil
	}
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) {
		return nil, ebpf.ErrKeyNotExist
	}

	var (
		gotIfindex uint32
		gotIP      string
		gotID      string
	)
	cubevsAddTAPDevice = func(ifindex uint32, ip net.IP, id string, version uint32, opts cubevs.MVMOptions) error {
		gotIfindex = ifindex
		gotIP = ip.String()
		gotID = id
		if version == 0 {
			t.Fatal("version=0, want incremented version")
		}
		if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != true {
			t.Fatalf("opts.AllowInternetAccess=%v, want true", opts.AllowInternetAccess)
		}
		return nil
	}

	svc := &localService{}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:  "sandbox-2",
			TapName:    "z192.168.0.8",
			TapIfIndex: 23,
			SandboxIP:  "192.168.0.8",
			CubeVSContext: &CubeVSContext{
				AllowInternetAccess: boolPtr(true),
			},
		},
	}

	if err := svc.refreshCubeVSTap(state); err != nil {
		t.Fatalf("refreshCubeVSTap error=%v", err)
	}
	if gotIfindex != 23 || gotIP != "192.168.0.8" || gotID != "sandbox-2" {
		t.Fatalf("AddTAPDevice got ifindex=%d ip=%s id=%s", gotIfindex, gotIP, gotID)
	}
}

func TestRefreshCubeVSTapPropagatesAttachFilterError(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldAdd := cubevsAddTAPDevice
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsAddTAPDevice = oldAdd
	})

	wantErr := errors.New("attach failed")
	cubevsAttachFilter = func(uint32) error { return wantErr }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) {
		t.Fatal("GetTAPDevice should not be called when attach fails")
		return nil, nil
	}
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		t.Fatal("AddTAPDevice should not be called when attach fails")
		return nil
	}

	svc := &localService{}
	state := &managedState{
		persistedState: persistedState{
			TapName:    "z192.168.0.9",
			TapIfIndex: 29,
			SandboxIP:  "192.168.0.9",
		},
	}

	err := svc.refreshCubeVSTap(state)
	if !errors.Is(err, wantErr) {
		t.Fatalf("refreshCubeVSTap error=%v, want %v", err, wantErr)
	}
}

func TestRecoverCleansOrphanTapsWithoutPersistedState(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
	})

	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.0.2": {
				Name:  "z192.168.0.2",
				Index: 12,
				IP:    net.ParseIP("192.168.0.2").To4(),
			},
		}, nil
	}
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		return &tapDevice{
			Name:  tap.Name,
			Index: tap.Index,
			IP:    tap.IP,
			File:  os.NewFile(uintptr(1), "/dev/null"),
		}, nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) { return map[uint16]cubevs.MVMPort{}, nil }

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	if len(svc.tapPool) != 1 {
		t.Fatalf("tapPool len=%d, want 1", len(svc.tapPool))
	}
	if svc.tapPool[0].Name != "z192.168.0.2" {
		t.Fatalf("tapPool[0]=%+v, want z192.168.0.2", svc.tapPool[0])
	}
}

func TestRecoverKeepsPersistedTapAndRemovesOnlyOrphans(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAdd := cubevsAddTAPDevice
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAdd
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		addARPEntryFunc = oldARP
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	persisted := &persistedState{
		SandboxID:     "sandbox-1",
		NetworkHandle: "sandbox-1",
		TapName:       "z192.168.0.3",
		TapIfIndex:    13,
		SandboxIP:     "192.168.0.3",
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.0.2": {
				Name:  "z192.168.0.2",
				Index: 12,
				IP:    net.ParseIP("192.168.0.2").To4(),
			},
			"192.168.0.3": {
				Name:  "z192.168.0.3",
				Index: 13,
				IP:    net.ParseIP("192.168.0.3").To4(),
			},
		}, nil
	}
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		return &tapDevice{
			Name:  tap.Name,
			Index: tap.Index,
			IP:    tap.IP,
			File:  os.NewFile(uintptr(1), "/dev/null"),
		}, nil
	}
	cubevsAttachFilter = func(uint32) error { return nil }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) {
		return &cubevs.TAPDevice{}, nil
	}
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		return nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) { return map[uint16]cubevs.MVMPort{}, nil }
	addARPEntryFunc = func(net.IP, string, int) error { return nil }
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return nil, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }
	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}

	svc := &localService{
		store:     store,
		allocator: allocator,
		ports:     &portAllocator{},
		cfg: Config{
			CIDR:       "192.168.0.0/18",
			MVMMacAddr: "20:90:6f:fc:fc:fc",
			MvmMtu:     1300,
		},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	if _, ok := svc.states["sandbox-1"]; !ok {
		t.Fatal("recover states missing sandbox-1")
	}
	if len(svc.tapPool) != 1 || svc.tapPool[0].Name != "z192.168.0.2" {
		t.Fatalf("tapPool=%+v, want free tap z192.168.0.2", svc.tapPool)
	}
}

func TestRecoverDropsStalePersistedStateWithoutBlockingStartup(t *testing.T) {
	oldList := listCubeTapsFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldDelTap := cubevsDelTAPDevice
	oldDelPort := cubevsDelPortMap
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsDelTAPDevice = oldDelTap
		cubevsDelPortMap = oldDelPort
	})

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	persisted := &persistedState{
		SandboxID:     "sandbox-stale",
		NetworkHandle: "sandbox-stale",
		TapName:       "z192.168.0.9",
		TapIfIndex:    19,
		SandboxIP:     "192.168.0.9",
		PortMappings: []PortMapping{
			{Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 61119, ContainerPort: 80},
		},
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{}, nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) {
		return []cubevs.TAPDevice{{
			IP:      net.ParseIP("192.168.0.9").To4(),
			Ifindex: 19,
		}}, nil
	}
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) {
		return map[uint16]cubevs.MVMPort{
			61119: {Ifindex: 19, ListenPort: 80},
		}, nil
	}
	delTapCalls := 0
	delPortCalls := 0
	cubevsDelTAPDevice = func(ifindex uint32, ip net.IP) error {
		delTapCalls++
		if ifindex != 19 || ip.String() != "192.168.0.9" {
			t.Fatalf("cubevsDelTAPDevice got ifindex=%d ip=%s", ifindex, ip)
		}
		return nil
	}
	cubevsDelPortMap = func(ifindex uint32, containerPort, hostPort uint16) error {
		delPortCalls++
		if ifindex != 19 || containerPort != 80 || hostPort != 61119 {
			t.Fatalf("cubevsDelPortMap got ifindex=%d containerPort=%d hostPort=%d", ifindex, containerPort, hostPort)
		}
		return nil
	}

	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	if delTapCalls != 1 {
		t.Fatalf("delTapCalls=%d, want 1", delTapCalls)
	}
	if delPortCalls != 1 {
		t.Fatalf("delPortCalls=%d, want 1", delPortCalls)
	}
	statePath, _ := store.path("sandbox-stale")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("stale state still exists after recover, stat err=%v", err)
	}
}

func TestEnsureReleaseEnsureReusesTapFromPool(t *testing.T) {
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

	created := 0
	newTapFunc = func(ip net.IP, _ string, _ int, _ int) (*tapDevice, error) {
		created++
		return &tapDevice{
			Name:  tapName(ip.String()),
			Index: 12,
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
	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{min: 10000, max: 10100, next: 10000, assigned: make(map[uint16]struct{})},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMInnerIP: "169.254.68.6", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmGwDestIP: "169.254.68.5", MvmMask: 30, MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}

	first, err := svc.EnsureNetwork(t.Context(), &EnsureNetworkRequest{SandboxID: "sandbox-1"})
	if err != nil {
		t.Fatalf("EnsureNetwork first error=%v", err)
	}
	if created != 1 {
		t.Fatalf("created=%d, want 1", created)
	}
	if _, err := svc.ReleaseNetwork(t.Context(), &ReleaseNetworkRequest{SandboxID: "sandbox-1"}); err != nil {
		t.Fatalf("ReleaseNetwork error=%v", err)
	}
	if len(svc.tapPool) != 1 {
		t.Fatalf("tapPool len=%d, want 1", len(svc.tapPool))
	}
	second, err := svc.EnsureNetwork(t.Context(), &EnsureNetworkRequest{SandboxID: "sandbox-2"})
	if err != nil {
		t.Fatalf("EnsureNetwork second error=%v", err)
	}
	if created != 1 {
		t.Fatalf("created=%d, want reuse from pool", created)
	}
	if first.PersistMetadata["sandbox_ip"] != second.PersistMetadata["sandbox_ip"] {
		t.Fatalf("sandbox_ip first=%s second=%s, want reuse same tap ip", first.PersistMetadata["sandbox_ip"], second.PersistMetadata["sandbox_ip"])
	}
}

func TestGetTapFileRestoresMissingFD(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAddTap := cubevsAddTAPDevice
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAddTap
		addARPEntryFunc = oldARP
		netlinkRouteListFiltered = oldRouteList
		netlinkRouteReplace = oldRouteReplace
	})

	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	persisted := &persistedState{
		SandboxID:     "sandbox-1",
		NetworkHandle: "sandbox-1",
		TapName:       "z192.168.0.3",
		TapIfIndex:    13,
		SandboxIP:     "192.168.0.3",
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("store.Save error=%v", err)
	}

	listCubeTapsFunc = func() (map[string]*tapDevice, error) {
		return map[string]*tapDevice{
			"192.168.0.3": {
				Name:  "z192.168.0.3",
				Index: 13,
				IP:    net.ParseIP("192.168.0.3").To4(),
			},
		}, nil
	}
	restoreCalls := 0
	restoreTapFunc = func(tap *tapDevice, _ int, _ string, _ int) (*tapDevice, error) {
		restoreCalls++
		tap.File = newTestTapFile(t)
		return tap, nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) { return map[uint16]cubevs.MVMPort{}, nil }
	cubevsAttachFilter = func(uint32) error { return nil }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) { return &cubevs.TAPDevice{}, nil }
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	addARPEntryFunc = func(net.IP, string, int) error { return nil }
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) { return nil, nil }
	netlinkRouteReplace = func(_ *netlink.Route) error { return nil }

	allocator, err := newIPAllocator("192.168.0.0/18")
	if err != nil {
		t.Fatalf("newIPAllocator error=%v", err)
	}
	svc := &localService{
		store:             store,
		allocator:         allocator,
		ports:             &portAllocator{},
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1300},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	svc.states["sandbox-1"].tap.File = nil
	file, err := svc.GetTapFile("sandbox-1", "z192.168.0.3")
	if err != nil {
		t.Fatalf("GetTapFile error=%v", err)
	}
	if file == nil {
		t.Fatal("GetTapFile returned nil file")
	}
	if restoreCalls < 2 {
		t.Fatalf("restoreCalls=%d, want at least 2 (recover + on-demand reopen)", restoreCalls)
	}
}

func TestListNetworksReturnsSortedManagedStates(t *testing.T) {
	svc := &localService{
		states: map[string]*managedState{
			"sandbox-b": {
				persistedState: persistedState{
					SandboxID:     "sandbox-b",
					NetworkHandle: "handle-b",
					TapName:       "z192.168.0.12",
					TapIfIndex:    12,
					SandboxIP:     "192.168.0.12",
					PortMappings: []PortMapping{{
						Protocol:      "tcp",
						HostIP:        "127.0.0.1",
						HostPort:      30012,
						ContainerPort: 80,
					}},
				},
			},
			"sandbox-a": {
				persistedState: persistedState{
					SandboxID:     "sandbox-a",
					NetworkHandle: "handle-a",
					TapName:       "z192.168.0.11",
					TapIfIndex:    11,
					SandboxIP:     "192.168.0.11",
				},
			},
		},
	}

	resp, err := svc.ListNetworks(t.Context(), &ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks error=%v", err)
	}
	if len(resp.Networks) != 2 {
		t.Fatalf("ListNetworks len=%d, want 2", len(resp.Networks))
	}
	if resp.Networks[0].SandboxID != "sandbox-a" || resp.Networks[1].SandboxID != "sandbox-b" {
		t.Fatalf("ListNetworks order=%+v, want sandbox-a then sandbox-b", resp.Networks)
	}
	if resp.Networks[1].TapName != "z192.168.0.12" || resp.Networks[1].TapIfIndex != 12 || resp.Networks[1].SandboxIP != "192.168.0.12" {
		t.Fatalf("ListNetworks sandbox-b=%+v", resp.Networks[1])
	}
	if len(resp.Networks[1].PortMappings) != 1 || resp.Networks[1].PortMappings[0].HostPort != 30012 {
		t.Fatalf("ListNetworks sandbox-b port mappings=%+v", resp.Networks[1].PortMappings)
	}
}

func newTestTapFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "tap-fd-*")
	if err != nil {
		t.Fatalf("CreateTemp error=%v", err)
	}
	return file
}

func boolPtr(v bool) *bool { return &v }

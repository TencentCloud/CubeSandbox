// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/vishvananda/netlink"
)

func TestCubeVSTapRegistration(t *testing.T) {
	opts := cubeVSTapRegistration(&CubeNetworkConfig{
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
	opts := cubeVSTapRegistration(&CubeNetworkConfig{
		AllowInternetAccess: boolPtr(false),
	})
	if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != false {
		t.Fatalf("opts.AllowInternetAccess=%v, want false", opts.AllowInternetAccess)
	}
}

func TestCubeVSTapRegistrationExtractsL7AllowOut(t *testing.T) {
	sni := "API.Example.COM."
	sniWildcard := "*.SNI.Example.COM"
	hostIP := "1.2.3.4:443"
	hostCIDR := "10.1.2.3/8"
	hostDomain := "Gateway.Example.COM:8443"
	hostWildcard := "*.Gateway.Example.COM"
	duplicateHost := "gateway.example.com"
	invalidHost := "999.999.999.999"
	opts := cubeVSTapRegistration(&CubeNetworkConfig{
		AllowOut: []string{"8.8.8.8"},
		Rules: []*EgressRule{
			{Match: &EgressRuleMatch{SNI: &sni, Host: &hostIP}},
			{Match: &EgressRuleMatch{Host: &hostCIDR}},
			{Match: &EgressRuleMatch{Host: &hostDomain}},
			{Match: &EgressRuleMatch{SNI: &sni}},
			{Match: &EgressRuleMatch{SNI: &sniWildcard}},
			{Match: &EgressRuleMatch{Host: &hostWildcard}},
			{Match: &EgressRuleMatch{Host: &duplicateHost}},
			{Match: &EgressRuleMatch{Host: &invalidHost}},
			{Match: &EgressRuleMatch{Path: stringPtr("/v1/chat")}},
		},
	})
	if opts.AllowOut == nil || len(*opts.AllowOut) != 1 || (*opts.AllowOut)[0] != "8.8.8.8" {
		t.Fatalf("opts.AllowOut=%v, want [8.8.8.8]", opts.AllowOut)
	}
	if opts.L7AllowOut == nil {
		t.Fatal("opts.L7AllowOut=nil, want extracted targets")
	}
	want := []string{"api.example.com", "1.2.3.4", "10.0.0.0/8", "gateway.example.com", "*.sni.example.com", "*.gateway.example.com"}
	if got := *opts.L7AllowOut; !slices.Equal(got, want) {
		t.Fatalf("opts.L7AllowOut=%v, want %v", got, want)
	}
}

func TestRefreshCubeVSTapForRecoverReattachesFilterWithoutPolicyUpsert(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldUpsert := cubevsUpsertTAPDevice
	oldUpsertMeta := cubevsUpsertTAPDeviceMeta
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsUpsertTAPDevice = oldUpsert
		cubevsUpsertTAPDeviceMeta = oldUpsertMeta
	})

	attachCalls := 0
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
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		t.Fatal("UpsertTAPDevice should not be called during recover")
		return nil
	}
	cubevsUpsertTAPDeviceMeta = func(uint32, net.IP, string, uint32) error {
		t.Fatal("UpsertTAPDeviceMeta should not be called when metadata exists")
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

	if err := svc.refreshCubeVSTapForRecover(state); err != nil {
		t.Fatalf("refreshCubeVSTapForRecover error=%v", err)
	}
	if attachCalls != 1 {
		t.Fatalf("AttachFilter calls=%d, want 1", attachCalls)
	}
}

func TestRefreshCubeVSTapForRecoverRestoresMissingMetadataOnly(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldUpsert := cubevsUpsertTAPDevice
	oldUpsertMeta := cubevsUpsertTAPDeviceMeta
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsUpsertTAPDevice = oldUpsert
		cubevsUpsertTAPDeviceMeta = oldUpsertMeta
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
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		t.Fatal("UpsertTAPDevice should not be called during recover")
		return nil
	}

	var (
		gotIfindex uint32
		gotIP      string
		gotID      string
	)
	cubevsUpsertTAPDeviceMeta = func(ifindex uint32, ip net.IP, id string, version uint32) error {
		gotIfindex = ifindex
		gotIP = ip.String()
		gotID = id
		if version == 0 {
			t.Fatal("version=0, want incremented version")
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
			CubeNetworkConfig: &CubeNetworkConfig{
				AllowInternetAccess: boolPtr(true),
			},
		},
	}

	if err := svc.refreshCubeVSTapForRecover(state); err != nil {
		t.Fatalf("refreshCubeVSTapForRecover error=%v", err)
	}
	if gotIfindex != 23 || gotIP != "192.168.0.8" || gotID != "sandbox-2" {
		t.Fatalf("UpsertTAPDeviceMeta got ifindex=%d ip=%s id=%s", gotIfindex, gotIP, gotID)
	}
}

func TestRefreshCubeVSTapForRecoverPropagatesAttachFilterError(t *testing.T) {
	oldAttach := cubevsAttachFilter
	oldGet := cubevsGetTAPDevice
	oldUpsert := cubevsUpsertTAPDevice
	oldUpsertMeta := cubevsUpsertTAPDeviceMeta
	t.Cleanup(func() {
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGet
		cubevsUpsertTAPDevice = oldUpsert
		cubevsUpsertTAPDeviceMeta = oldUpsertMeta
	})

	wantErr := errors.New("attach failed")
	cubevsAttachFilter = func(uint32) error { return wantErr }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) {
		t.Fatal("GetTAPDevice should not be called when attach fails")
		return nil, nil
	}
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		t.Fatal("UpsertTAPDevice should not be called when attach fails")
		return nil
	}
	cubevsUpsertTAPDeviceMeta = func(uint32, net.IP, string, uint32) error {
		t.Fatal("UpsertTAPDeviceMeta should not be called when attach fails")
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

	err := svc.refreshCubeVSTapForRecover(state)
	if !errors.Is(err, wantErr) {
		t.Fatalf("refreshCubeVSTapForRecover error=%v, want %v", err, wantErr)
	}
}

func TestRecoverCleansOrphanTapsWithoutPersistedState(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		cubevsPrepareTAPPolicy = oldPrepareTap
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
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }

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
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1500},
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
	oldUpsert := cubevsUpsertTAPDevice
	oldUpsertMeta := cubevsUpsertTAPDeviceMeta
	oldPrepareTap := cubevsPrepareTAPPolicy
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
		cubevsUpsertTAPDevice = oldUpsert
		cubevsUpsertTAPDeviceMeta = oldUpsertMeta
		cubevsPrepareTAPPolicy = oldPrepareTap
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
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error {
		t.Fatal("UpsertTAPDevice should not be called during recover")
		return nil
	}
	cubevsUpsertTAPDeviceMeta = func(uint32, net.IP, string, uint32) error { return nil }
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }
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
			MvmMtu:     1500,
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
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1500},
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
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldAddPort := cubevsAddPortMap
	oldDelPort := cubevsDelPortMap
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
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }
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
		cfg:               Config{CIDR: "192.168.0.0/18", MVMInnerIP: "169.254.68.6", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmGwDestIP: "169.254.68.5", MvmMask: 30, MvmMtu: 1500},
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
	if len(svc.tapPool) != 0 {
		t.Fatalf("tapPool len=%d, want 0 before async preparation", len(svc.tapPool))
	}
	if len(svc.abnormalTapPool) != 1 {
		t.Fatalf("abnormalTapPool len=%d, want 1 pending async preparation", len(svc.abnormalTapPool))
	}
	svc.handleAbnormalTaps()
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
	oldOpen := openTapFdByNameFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAddTap := cubevsAddTAPDevice
	oldUpsert := cubevsUpsertTAPDevice
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		openTapFdByNameFunc = oldOpen
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAddTap
		cubevsUpsertTAPDevice = oldUpsert
		cubevsPrepareTAPPolicy = oldPrepareTap
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
	openCalls := 0
	openTapFdByNameFunc = func(string) (*os.File, error) {
		openCalls++
		return newTestTapFile(t), nil
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) { return map[uint16]cubevs.MVMPort{}, nil }
	cubevsAttachFilter = func(uint32) error { return nil }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) { return &cubevs.TAPDevice{}, nil }
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }
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
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1500},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	svc.states["sandbox-1"].tap.File = nil
	file, ifindex, err := svc.GetTapFile("sandbox-1", "z192.168.0.3")
	if err != nil {
		t.Fatalf("GetTapFile error=%v", err)
	}
	if file == nil {
		t.Fatal("GetTapFile returned nil file")
	}
	if ifindex != 13 {
		t.Fatalf("ifindex=%d, want 13", ifindex)
	}
	// recover() does the full restore once; the on-demand reopen of an idle,
	// already-configured managed tap takes the cheap openTapFdByName path and
	// must NOT trigger another full restoreTap.
	if restoreCalls != 1 {
		t.Fatalf("restoreCalls=%d, want 1 (recover only)", restoreCalls)
	}
	if openCalls != 1 {
		t.Fatalf("openCalls=%d, want 1 (on-demand reopen)", openCalls)
	}
}

// TestGetTapFileHotPathOpenFailureSelfHeals asserts that when the cheap reopen
// (openTapFdByName) fails transiently, GetTapFile does NOT propagate the error
// but falls through to the restoreTap recovery path, which re-validates and
// retries fd acquisition so the sandbox create self-heals.
func TestGetTapFileHotPathOpenFailureSelfHeals(t *testing.T) {
	oldList := listCubeTapsFunc
	oldRestore := restoreTapFunc
	oldOpen := openTapFdByNameFunc
	oldListCubeVSTaps := cubevsListTAPDevices
	oldListPortMappings := cubevsListPortMappings
	oldAttach := cubevsAttachFilter
	oldGetTap := cubevsGetTAPDevice
	oldAddTap := cubevsAddTAPDevice
	oldUpsert := cubevsUpsertTAPDevice
	oldPrepareTap := cubevsPrepareTAPPolicy
	oldARP := addARPEntryFunc
	oldRouteList := netlinkRouteListFiltered
	oldRouteReplace := netlinkRouteReplace
	t.Cleanup(func() {
		listCubeTapsFunc = oldList
		restoreTapFunc = oldRestore
		openTapFdByNameFunc = oldOpen
		cubevsListTAPDevices = oldListCubeVSTaps
		cubevsListPortMappings = oldListPortMappings
		cubevsAttachFilter = oldAttach
		cubevsGetTAPDevice = oldGetTap
		cubevsAddTAPDevice = oldAddTap
		cubevsUpsertTAPDevice = oldUpsert
		cubevsPrepareTAPPolicy = oldPrepareTap
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
	openCalls := 0
	openTapFdByNameFunc = func(string) (*os.File, error) {
		openCalls++
		return nil, fmt.Errorf("device or resource busy")
	}
	cubevsListTAPDevices = func() ([]cubevs.TAPDevice, error) { return nil, nil }
	cubevsListPortMappings = func() (map[uint16]cubevs.MVMPort, error) { return map[uint16]cubevs.MVMPort{}, nil }
	cubevsAttachFilter = func(uint32) error { return nil }
	cubevsGetTAPDevice = func(uint32) (*cubevs.TAPDevice, error) { return &cubevs.TAPDevice{}, nil }
	cubevsAddTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsUpsertTAPDevice = func(uint32, net.IP, string, uint32, cubevs.MVMOptions) error { return nil }
	cubevsPrepareTAPPolicy = func(uint32) error { return nil }
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
		cfg:               Config{CIDR: "192.168.0.0/18", MVMMacAddr: "20:90:6f:fc:fc:fc", MvmMtu: 1500},
		cubeDev:           &cubeDev{Index: 16},
		states:            make(map[string]*managedState),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := svc.recover(); err != nil {
		t.Fatalf("recover error=%v", err)
	}
	svc.states["sandbox-1"].tap.File = nil
	file, ifindex, err := svc.GetTapFile("sandbox-1", "z192.168.0.3")
	if err != nil {
		t.Fatalf("GetTapFile error=%v", err)
	}
	if file == nil {
		t.Fatal("GetTapFile returned nil file")
	}
	if ifindex != 13 {
		t.Fatalf("ifindex=%d, want 13", ifindex)
	}
	if openCalls != 1 {
		t.Fatalf("openCalls=%d, want 1 (one failed fast-reopen attempt)", openCalls)
	}
	// recover() restores once; the failed fast reopen must fall through to a
	// second restoreTap instead of propagating the error.
	if restoreCalls != 2 {
		t.Fatalf("restoreCalls=%d, want 2 (recover + self-heal fallback)", restoreCalls)
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

func stringPtr(v string) *string { return &v }

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
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	cubevsAttachFilter = cubevs.AttachFilter
	cubevsGetTAPDevice = cubevs.GetTAPDevice
	cubevsAddTAPDevice = cubevs.AddTAPDevice
	cubevsDelTAPDevice = cubevs.DelTAPDevice
	cubevsAddPortMap   = cubevs.AddPortMapping
	cubevsDelPortMap   = cubevs.DelPortMapping
	listCubeTapsFunc   = listCubeTaps
	getTapByNameFunc   = getTapByName
	destroyTapFunc     = destroyTap
	addARPEntryFunc    = addARPEntry
)

type managedState struct {
	persistedState
	tap     *tapDevice
	proxies []*hostProxy
}

type localService struct {
	cfg       Config
	store     *stateStore
	allocator *ipAllocator
	ports     *portAllocator
	device    *machineDevice
	cubeDev   *cubeDev

	mu                sync.Mutex
	states            map[string]*managedState
	tapPool           []*tapDevice
	abnormalTapPool   []*tapDevice
	quarantinedTaps   map[string]*tapDevice
	destroyFailedTaps map[string]*tapDevice

	version uint32
}

func NewLocalService(cfg Config) (Service, error) {
	if cfg.EthName == "" {
		return nil, fmt.Errorf("network-agent requires explicit eth_name from cubelet config or flag")
	}
	store, err := newStateStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	allocator, err := newIPAllocator(cfg.CIDR)
	if err != nil {
		return nil, err
	}
	ports, err := newPortAllocator()
	if err != nil {
		return nil, err
	}
	device, err := getMachineDevice(cfg.EthName)
	if err != nil {
		return nil, err
	}
	err = disableGRO(cfg.EthName)
	if err != nil {
		CubeLog.WithContext(context.Background()).Warnf("network-agent failed to disable GRO on %s: %v", cfg.EthName, err)
	}
	cdev, err := getOrCreateCubeDev(allocator.GatewayIP(), allocator.mask, cfg.MvmMtu, cfg.MvmGwMacAddr)
	if err != nil {
		return nil, err
	}
	if err := ensureRouteToCubeDev(cfg.CIDR, cdev); err != nil {
		return nil, err
	}
	mvmInnerIP := net.ParseIP(cfg.MVMInnerIP).To4()
	mvmMacAddr, err := net.ParseMAC(cfg.MVMMacAddr)
	if err != nil {
		return nil, err
	}
	mvmGatewayIP := net.ParseIP(cfg.MvmGwDestIP).To4()
	params := cubevs.Params{
		MVMInnerIP:         mvmInnerIP,
		MVMMacAddr:         mvmMacAddr,
		MVMGatewayIP:       mvmGatewayIP,
		Cubegw0Ifindex:     uint32(cdev.Index),
		Cubegw0IP:          cdev.IP,
		Cubegw0MacAddr:     cdev.Mac,
		NodeIfindex:        uint32(device.Index),
		NodeIP:             device.IP,
		NodeMacAddr:        device.Mac,
		NodeGatewayMacAddr: device.GatewayMac,
	}
	if err := cubevs.Init(params); err != nil {
		return nil, err
	}
	if err := cubevs.SetSNATIPs([]*cubevs.SNATIP{{
		Ifindex: device.Index,
		IP:      device.IP,
	}}); err != nil {
		return nil, fmt.Errorf("set default local egress snat ip failed: %w", err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_local_port_range", []byte("10000\t19999"), 0644); err != nil {
		return nil, fmt.Errorf("set ip_local_port_range failed: %w", err)
	}
	sessionEvents := cubevs.StartSessionReaper()
	go func() {
		logger := CubeLog.WithContext(context.Background())
		for event := range sessionEvents {
			if event.Error != nil {
				logger.Warnf("cubevs session reaper: %v: %s", event.Error, event.Message)
			}
		}
	}()
	s := &localService{
		cfg:               cfg,
		store:             store,
		allocator:         allocator,
		ports:             ports,
		device:            device,
		cubeDev:           cdev,
		states:            make(map[string]*managedState),
		tapPool:           make([]*tapDevice, 0, cfg.TapInitNum),
		abnormalTapPool:   make([]*tapDevice, 0),
		quarantinedTaps:   make(map[string]*tapDevice),
		destroyFailedTaps: make(map[string]*tapDevice),
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	if err := s.ensureTapInventory(); err != nil {
		return nil, err
	}
	s.startMaintenanceLoop()
	return s, nil
}

func (s *localService) EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error) {
	CubeLog.WithContext(ctx).Infof(
		"network-agent EnsureNetwork request: sandbox_id=%s idempotency_key=%s interfaces=%d routes=%d arps=%d port_mappings=%d cubevs_context=%s persist_metadata=%v",
		req.SandboxID,
		req.IdempotencyKey,
		len(req.Interfaces),
		len(req.Routes),
		len(req.ARPNeighbors),
		len(req.PortMappings),
		formatCubeVSContext(req.CubeVSContext),
		req.PersistMetadata,
	)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.states[req.SandboxID]; ok {
		return existing.ensureResponse(), nil
	}
	state, err := s.createStateLocked(ctx, req)
	if err != nil {
		return nil, err
	}
	s.states[state.SandboxID] = state
	return state.ensureResponse(), nil
}

func (s *localService) ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error) {
	s.mu.Lock()
	state, ok := s.lookupStateLocked(req.SandboxID, req.NetworkHandle)
	if !ok {
		s.mu.Unlock()
		return &ReleaseNetworkResponse{Released: true, PersistMetadata: req.PersistMetadata}, nil
	}
	delete(s.states, state.SandboxID)
	s.mu.Unlock()

	if err := s.releaseState(ctx, state); err != nil {
		s.mu.Lock()
		s.states[state.SandboxID] = state
		s.mu.Unlock()
		return nil, err
	}
	return &ReleaseNetworkResponse{
		Released:        true,
		PersistMetadata: state.PersistMetadata,
	}, nil
}

func (s *localService) ReconcileNetwork(ctx context.Context, req *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error) {
	s.mu.Lock()
	state, ok := s.lookupStateLocked(req.SandboxID, req.NetworkHandle)
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("network %q not found", req.SandboxID)
	}
	if err := s.reconcileState(ctx, state); err != nil {
		return nil, err
	}
	return &ReconcileNetworkResponse{
		SandboxID:       state.SandboxID,
		NetworkHandle:   state.NetworkHandle,
		Converged:       true,
		Interfaces:      slices.Clone(state.Interfaces),
		Routes:          slices.Clone(state.Routes),
		ARPNeighbors:    slices.Clone(state.ARPNeighbors),
		PortMappings:    slices.Clone(state.PortMappings),
		PersistMetadata: cloneStringMap(state.PersistMetadata),
	}, nil
}

func (s *localService) GetNetwork(ctx context.Context, req *GetNetworkRequest) (*GetNetworkResponse, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.lookupStateLocked(req.SandboxID, req.NetworkHandle)
	if !ok {
		return nil, fmt.Errorf("network %q not found", req.SandboxID)
	}
	return &GetNetworkResponse{
		SandboxID:       state.SandboxID,
		NetworkHandle:   state.NetworkHandle,
		Interfaces:      slices.Clone(state.Interfaces),
		Routes:          slices.Clone(state.Routes),
		ARPNeighbors:    slices.Clone(state.ARPNeighbors),
		PortMappings:    slices.Clone(state.PortMappings),
		PersistMetadata: cloneStringMap(state.PersistMetadata),
	}, nil
}

func (s *localService) ListNetworks(ctx context.Context, req *ListNetworksRequest) (*ListNetworksResponse, error) {
	_ = ctx
	_ = req
	s.mu.Lock()
	defer s.mu.Unlock()

	networks := make([]NetworkState, 0, len(s.states))
	for _, state := range s.states {
		networks = append(networks, NetworkState{
			SandboxID:     state.SandboxID,
			NetworkHandle: state.NetworkHandle,
			TapName:       state.TapName,
			TapIfIndex:    int32(state.TapIfIndex),
			SandboxIP:     state.SandboxIP,
			PortMappings:  slices.Clone(state.PortMappings),
		})
	}
	slices.SortFunc(networks, func(a, b NetworkState) int {
		if a.SandboxID < b.SandboxID {
			return -1
		}
		if a.SandboxID > b.SandboxID {
			return 1
		}
		return 0
	})
	return &ListNetworksResponse{Networks: networks}, nil
}

func (s *localService) Health(ctx context.Context) error {
	_ = ctx
	return nil
}

func (s *localService) createStateLocked(ctx context.Context, req *EnsureNetworkRequest) (*managedState, error) {
	if err := s.ensureHostRoute(); err != nil {
		return nil, err
	}
	requestedMappings := s.normalizePortMappings(req.PortMappings)
	tap := s.dequeueTapLocked()
	fromPool := tap != nil
	if !fromPool {
		ip, err := s.allocator.Allocate()
		if err != nil {
			return nil, err
		}
		if err := s.cleanupConflictingTap(ip); err != nil {
			s.allocator.Release(ip)
			return nil, err
		}
		tap, err = newTapFunc(ip, s.cfg.MVMMacAddr, s.cfg.MvmMtu, s.cubeDev.Index)
		if err != nil {
			s.allocator.Release(ip)
			return nil, err
		}
	}
	actualMappings, err := s.configurePortMappings(tap, requestedMappings)
	if err != nil {
		if fromPool {
			s.recycleTapLocked(tap)
		} else {
			closeTapFile(tap.File)
			_ = destroyTapFunc(tap.Index)
			s.allocator.Release(tap.IP)
		}
		return nil, err
	}
	if err := s.registerCubeVSTap(tap.Index, tap.IP, req.SandboxID, req.CubeVSContext); err != nil {
		s.clearPortMappings(tap)
		if fromPool {
			s.recycleTapLocked(tap)
		} else {
			closeTapFile(tap.File)
			_ = destroyTapFunc(tap.Index)
			s.allocator.Release(tap.IP)
		}
		return nil, err
	}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:       req.SandboxID,
			NetworkHandle:   req.SandboxID,
			TapName:         tap.Name,
			TapIfIndex:      tap.Index,
			SandboxIP:       tap.IP.String(),
			Interfaces:      s.actualInterfaces(tap.Name, req.Interfaces),
			Routes:          slices.Clone(req.Routes),
			ARPNeighbors:    slices.Clone(req.ARPNeighbors),
			PortMappings:    actualMappings,
			CubeVSContext:   cloneCubeVSContext(req.CubeVSContext),
			PersistMetadata: s.persistMetadata(req.PersistMetadata, tap.Name, tap.IP.String()),
		},
		tap: tap,
	}
	if err := s.store.Save(&state.persistedState); err != nil {
		_ = cubevsDelTAPDevice(uint32(tap.Index), tap.IP.To4())
		s.clearPortMappings(tap)
		if fromPool {
			s.recycleTapLocked(tap)
		} else {
			closeTapFile(tap.File)
			_ = destroyTapFunc(tap.Index)
			s.allocator.Release(tap.IP)
		}
		return nil, err
	}
	return state, nil
}

func (s *localService) reconcileState(ctx context.Context, state *managedState) error {
	if err := s.ensureHostRoute(); err != nil {
		return err
	}
	if state.tap == nil || state.tap.File == nil {
		baseTap := state.tap
		if baseTap == nil {
			baseTap = &tapDevice{
				Name:         state.TapName,
				IP:           net.ParseIP(state.SandboxIP).To4(),
				PortMappings: append([]PortMapping(nil), state.PortMappings...),
			}
		} else {
			baseTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
		}
		tap, err := restoreTapFunc(baseTap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
		if err != nil {
			return err
		}
		state.tap = tap
	}
	s.allocator.Assign(net.ParseIP(state.SandboxIP).To4())
	for _, mapping := range state.PortMappings {
		s.ports.Assign(uint16(mapping.HostPort))
	}
	if err := s.refreshCubeVSTap(state); err != nil {
		return err
	}
	if err := addARPEntryFunc(net.ParseIP(state.SandboxIP).To4(), s.cfg.MVMMacAddr, s.cubeDev.Index); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	for _, mapping := range state.PortMappings {
		if err := cubevsAddPortMap(uint32(state.TapIfIndex), uint16(mapping.ContainerPort), uint16(mapping.HostPort)); err != nil {
			return err
		}
	}
	return nil
}

func (s *localService) refreshCubeVSTap(state *managedState) error {
	// Re-attach the ingress filter for recovered TAPs so the running kernel
	// always uses the currently deployed from_cube program.
	if err := cubevsAttachFilter(uint32(state.TapIfIndex)); err != nil {
		return fmt.Errorf("attach cubevs filter for tap %s(%d): %w", state.TapName, state.TapIfIndex, err)
	}
	if _, err := cubevsGetTAPDevice(uint32(state.TapIfIndex)); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			// The tap map can be gone after restart; recreate it for both not-exist and generic lookup failures.
		}
		if err := s.registerCubeVSTap(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4(), state.SandboxID, state.CubeVSContext); err != nil {
			return err
		}
	}
	return nil
}

func (s *localService) releaseState(ctx context.Context, state *managedState) error {
	_ = ctx
	for _, proxy := range state.proxies {
		_ = proxy.Close()
	}
	state.proxies = nil
	if state.tap == nil {
		state.tap = &tapDevice{
			Index:        state.TapIfIndex,
			Name:         state.TapName,
			IP:           net.ParseIP(state.SandboxIP).To4(),
			PortMappings: append([]PortMapping(nil), state.PortMappings...),
		}
	} else {
		state.tap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
	}
	s.clearPortMappings(state.tap)
	if err := cubevsDelTAPDevice(uint32(state.TapIfIndex), net.ParseIP(state.SandboxIP).To4()); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	s.mu.Lock()
	s.recycleTapLocked(state.tap)
	s.mu.Unlock()
	return s.store.Delete(state.SandboxID)
}

func (s *localService) recover() error {
	states, err := s.store.LoadAll()
	if err != nil {
		return err
	}
	taps, err := listCubeTapsFunc()
	if err != nil {
		return err
	}
	livePortMappings, err := cubevsListPortMappings()
	if err != nil {
		return err
	}
	mappingsByIfindex := make(map[int][]PortMapping)
	for hostPort, mapping := range livePortMappings {
		s.ports.Assign(hostPort)
		mappingsByIfindex[int(mapping.Ifindex)] = append(mappingsByIfindex[int(mapping.Ifindex)], PortMapping{
			Protocol:      "tcp",
			HostIP:        s.cfg.HostProxyBindIP,
			HostPort:      int32(hostPort),
			ContainerPort: int32(mapping.ListenPort),
		})
	}
	liveCubeVSTaps, err := cubevsListTAPDevices()
	if err != nil {
		return err
	}
	liveCubeVSTapsByIP := make(map[string]cubevs.TAPDevice, len(liveCubeVSTaps))
	for _, device := range liveCubeVSTaps {
		liveCubeVSTapsByIP[device.IP.String()] = device
	}
	statesByTapName := make(map[string]*persistedState, len(states))
	for _, state := range states {
		statesByTapName[state.TapName] = state
	}
	recovered := make(map[string]struct{}, len(states))
	for _, tap := range taps {
		s.allocator.Assign(tap.IP)
		tap.PortMappings = append([]PortMapping(nil), mappingsByIfindex[tap.Index]...)
		restoredTap, err := restoreTapFunc(tap, s.cfg.MvmMtu, s.cfg.MVMMacAddr, s.cubeDev.Index)
		if err != nil {
			s.enqueueAbnormalLocked(tap, abnormalStageRecoverRestore, err)
			continue
		}
		restoredTap.PortMappings = append([]PortMapping(nil), mappingsByIfindex[restoredTap.Index]...)
		if state, ok := statesByTapName[restoredTap.Name]; ok {
			managed := &managedState{persistedState: *state, tap: restoredTap}
			managed.TapIfIndex = restoredTap.Index
			managed.TapName = restoredTap.Name
			managed.SandboxIP = restoredTap.IP.String()
			if len(restoredTap.PortMappings) > 0 {
				managed.PortMappings = append([]PortMapping(nil), restoredTap.PortMappings...)
			}
			if managed.PersistMetadata == nil {
				managed.PersistMetadata = s.persistMetadata(nil, restoredTap.Name, restoredTap.IP.String())
			}
			if err := s.reconcileState(context.Background(), managed); err != nil {
				return err
			}
			s.states[managed.SandboxID] = managed
			recovered[managed.SandboxID] = struct{}{}
			continue
		}
		device, inCubeVS := liveCubeVSTapsByIP[restoredTap.IP.String()]
		if inCubeVS && restoredTap.InUse {
			managed := buildRecoveredState(restoredTap, &device, restoredTap.PortMappings, s.cfg)
			if err := s.reconcileState(context.Background(), managed); err != nil {
				return err
			}
			if err := s.store.Save(&managed.persistedState); err != nil {
				return err
			}
			s.states[managed.SandboxID] = managed
			recovered[managed.SandboxID] = struct{}{}
			continue
		}
		if inCubeVS {
			s.clearPortMappings(restoredTap)
			if err := cubevsDelTAPDevice(uint32(restoredTap.Index), restoredTap.IP.To4()); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				s.enqueueAbnormalLocked(restoredTap, abnormalStageRecoverCleanup, err)
				continue
			}
		}
		s.stageTapForPoolLocked(restoredTap, "recover")
	}
	for _, state := range states {
		if _, ok := recovered[state.SandboxID]; ok {
			continue
		}
		if device, ok := liveCubeVSTapsByIP[state.SandboxIP]; ok {
			staleTap := &tapDevice{
				Index:        device.Ifindex,
				Name:         state.TapName,
				IP:           net.ParseIP(state.SandboxIP).To4(),
				PortMappings: append([]PortMapping(nil), mappingsByIfindex[device.Ifindex]...),
			}
			if len(staleTap.PortMappings) == 0 {
				staleTap.PortMappings = append([]PortMapping(nil), state.PortMappings...)
			}
			CubeLog.WithContext(context.Background()).Warnf("network-agent recover dropping stale state for sandbox %s: tap %s missing on host, cleaning persisted state and cubevs entries", state.SandboxID, state.TapName)
			s.clearPortMappings(staleTap)
			if err := cubevsDelTAPDevice(uint32(device.Ifindex), staleTap.IP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				return err
			}
		}
		if err := s.store.Delete(state.SandboxID); err != nil {
			return err
		}
	}
	return nil
}

func (s *localService) cleanupConflictingTap(ip net.IP) error {
	taps, err := listCubeTapsFunc()
	if err != nil {
		return err
	}
	tap, ok := taps[ip.String()]
	if !ok {
		return nil
	}
	for _, state := range s.states {
		if state.TapName == tap.Name || state.SandboxIP == ip.String() {
			return fmt.Errorf("tap %s(%d) is still allocated to sandbox %s", tap.Name, tap.Index, state.SandboxID)
		}
	}
	for _, pooledTap := range s.tapPool {
		if pooledTap != nil && (pooledTap.Name == tap.Name || pooledTap.IP.Equal(ip)) {
			return fmt.Errorf("tap %s(%d) is already in free pool", tap.Name, tap.Index)
		}
	}
	for _, abnormalTap := range s.abnormalTapPool {
		if abnormalTap != nil && (abnormalTap.Name == tap.Name || abnormalTap.IP.Equal(ip)) {
			return fmt.Errorf("tap %s(%d) is pending abnormal cleanup", tap.Name, tap.Index)
		}
	}
	for _, quarantinedTap := range s.quarantinedTaps {
		if quarantinedTap != nil && (quarantinedTap.Name == tap.Name || quarantinedTap.IP.Equal(ip)) {
			return fmt.Errorf("tap %s(%d) is quarantined and unavailable for reuse", tap.Name, tap.Index)
		}
	}
	if err := destroyTapFunc(tap.Index); err != nil {
		return fmt.Errorf("destroy stale tap %s(%d): %w", tap.Name, tap.Index, err)
	}
	return nil
}

func (s *localService) cleanupOrphanTaps(states []*persistedState) error {
	taps, err := listCubeTapsFunc()
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(states))
	for _, state := range states {
		if state == nil {
			continue
		}
		name := state.TapName
		if name == "" && state.SandboxIP != "" {
			name = tapName(state.SandboxIP)
		}
		if name != "" {
			expected[name] = struct{}{}
		}
	}
	for _, tap := range taps {
		if _, ok := expected[tap.Name]; ok {
			continue
		}
		if err := destroyTapFunc(tap.Index); err != nil {
			return fmt.Errorf("destroy orphan tap %s(%d): %w", tap.Name, tap.Index, err)
		}
	}
	return nil
}

func (s *localService) normalizePortMappings(req []PortMapping) []PortMapping {
	byContainerPort := make(map[int32]PortMapping)
	for _, port := range s.cfg.DefaultExposedPorts {
		byContainerPort[int32(port)] = PortMapping{
			Protocol:      "tcp",
			HostIP:        s.cfg.HostProxyBindIP,
			ContainerPort: int32(port),
		}
	}
	for _, mapping := range req {
		if mapping.ContainerPort == 0 {
			continue
		}
		if mapping.HostIP == "" {
			mapping.HostIP = s.cfg.HostProxyBindIP
		}
		if mapping.Protocol == "" {
			mapping.Protocol = "tcp"
		}
		byContainerPort[mapping.ContainerPort] = mapping
	}
	ports := make([]int, 0, len(byContainerPort))
	for containerPort := range byContainerPort {
		ports = append(ports, int(containerPort))
	}
	slices.Sort(ports)
	result := make([]PortMapping, 0, len(ports))
	for _, containerPort := range ports {
		result = append(result, byContainerPort[int32(containerPort)])
	}
	return result
}

func (s *localService) actualInterfaces(tapName string, req []Interface) []Interface {
	if len(req) == 0 {
		return []Interface{{
			Name:    tapName,
			MAC:     s.cfg.MVMMacAddr,
			MTU:     int32(s.cfg.MvmMtu),
			IPs:     []string{fmt.Sprintf("%s/%d", s.cfg.MVMInnerIP, s.cfg.MvmMask)},
			Gateway: s.cfg.MvmGwDestIP,
		}}
	}
	out := slices.Clone(req)
	out[0].Name = tapName
	if out[0].MAC == "" {
		out[0].MAC = s.cfg.MVMMacAddr
	}
	if out[0].MTU == 0 {
		out[0].MTU = int32(s.cfg.MvmMtu)
	}
	if len(out[0].IPs) == 0 {
		out[0].IPs = []string{fmt.Sprintf("%s/%d", s.cfg.MVMInnerIP, s.cfg.MvmMask)}
	}
	if out[0].Gateway == "" {
		out[0].Gateway = s.cfg.MvmGwDestIP
	}
	return out
}

func (s *localService) persistMetadata(base map[string]string, tapName string, sandboxIP string) map[string]string {
	metadata := cloneStringMap(base)
	metadata["sandbox_ip"] = sandboxIP
	metadata["host_tap_name"] = tapName
	metadata["mvm_inner_ip"] = s.cfg.MVMInnerIP
	metadata["gateway_ip"] = s.cfg.MvmGwDestIP
	return metadata
}

func (s *localService) ensureHostRoute() error {
	return ensureRouteToCubeDev(s.cfg.CIDR, s.cubeDev)
}

func cloneCubeVSContext(in *CubeVSContext) *CubeVSContext {
	if in == nil {
		return nil
	}
	out := &CubeVSContext{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
	}
	if in.AllowInternetAccess != nil {
		v := *in.AllowInternetAccess
		out.AllowInternetAccess = &v
	}
	return out
}

func formatCubeVSContext(in *CubeVSContext) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[]"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *in.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v", allowInternetAccess, in.AllowOut, in.DenyOut)
}

func (s *localService) registerCubeVSTap(ifindex int, ip net.IP, sandboxID string, ctx *CubeVSContext) error {
	opts := cubeVSTapRegistration(ctx)
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent register cubevs tap: sandbox_id=%s ifindex=%d sandbox_ip=%s cubevs_context=%s allow_internet_access=%v allow_out=%v deny_out=%v",
		sandboxID,
		ifindex,
		ip.String(),
		formatCubeVSContext(ctx),
		opts.AllowInternetAccess,
		opts.AllowOut,
		opts.DenyOut,
	)
	return cubevsAddTAPDevice(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1), opts)
}

func cubeVSTapRegistration(ctx *CubeVSContext) cubevs.MVMOptions {
	if ctx == nil {
		allowInternetAccess := true
		return cubevs.MVMOptions{AllowInternetAccess: &allowInternetAccess}
	}
	opts := cubevs.MVMOptions{}
	if ctx.AllowInternetAccess != nil {
		v := *ctx.AllowInternetAccess
		opts.AllowInternetAccess = &v
	} else {
		allowInternetAccess := true
		opts.AllowInternetAccess = &allowInternetAccess
	}
	if len(ctx.AllowOut) > 0 {
		allowOut := append([]string(nil), ctx.AllowOut...)
		opts.AllowOut = &allowOut
	}
	if len(ctx.DenyOut) > 0 {
		denyOut := append([]string(nil), ctx.DenyOut...)
		opts.DenyOut = &denyOut
	}
	return opts
}

func (s *localService) startProxies(state *managedState) error {
	guestIP, err := firstGuestIP(state.Interfaces)
	if err != nil {
		return err
	}
	state.proxies = nil
	for _, mapping := range state.PortMappings {
		proxy, err := newHostProxy(
			nonEmpty(mapping.HostIP, s.cfg.HostProxyBindIP),
			mapping.HostPort,
			state.TapName,
			guestIP,
			mapping.ContainerPort,
			int(s.cfg.ConnectTimeout.Seconds()),
		)
		if err != nil {
			for _, existing := range state.proxies {
				_ = existing.Close()
			}
			state.proxies = nil
			return err
		}
		state.proxies = append(state.proxies, proxy)
	}
	return nil
}

func (s *localService) lookupStateLocked(sandboxID, networkHandle string) (*managedState, bool) {
	if sandboxID != "" {
		state, ok := s.states[sandboxID]
		return state, ok
	}
	if networkHandle != "" {
		state, ok := s.states[networkHandle]
		return state, ok
	}
	return nil, false
}

func (s *managedState) ensureResponse() *EnsureNetworkResponse {
	return &EnsureNetworkResponse{
		SandboxID:       s.SandboxID,
		NetworkHandle:   s.NetworkHandle,
		Interfaces:      slices.Clone(s.Interfaces),
		Routes:          slices.Clone(s.Routes),
		ARPNeighbors:    slices.Clone(s.ARPNeighbors),
		PortMappings:    slices.Clone(s.PortMappings),
		PersistMetadata: cloneStringMap(s.PersistMetadata),
	}
}

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

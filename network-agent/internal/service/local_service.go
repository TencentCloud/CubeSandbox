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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	cubevsAttachFilter        = cubevs.AttachFilter
	cubevsGetTAPDevice        = cubevs.GetTAPDevice
	cubevsAddTAPDevice        = cubevs.AddTAPDevice
	cubevsUpsertTAPDevice     = cubevs.UpsertTAPDevice
	cubevsUpsertTAPDeviceMeta = cubevs.UpsertTAPDeviceMeta
	cubevsDelTAPDevice        = cubevs.DelTAPDevice
	cubevsPrepareTAPPolicy    = cubevs.PrepareTAPDevicePolicy
	cubevsAddPortMap          = cubevs.AddPortMapping
	cubevsDelPortMap          = cubevs.DelPortMapping
	listCubeTapsFunc          = listCubeTaps
	getTapByNameFunc          = getTapByName
	destroyTapFunc            = destroyTap
	addARPEntryFunc           = addARPEntry
)

type managedState struct {
	persistedState
	tap     *tapDevice
	proxies []*hostProxy

	// policyKnown is an in-memory guard that says CubeNetworkConfig is a
	// complete desired policy. Recovered live TAPs may only have runtime
	// metadata, so recovery must not replay an unknown policy back to eBPF maps.
	policyKnown bool

	// pendingEgressPush is set when a per-sandbox PUT to CubeEgress's
	// admin API has failed transiently (5xx, transport error). The
	// maintenance loop retries until the push lands or until the
	// sandbox is released. Permanent failures (4xx) clear the flag —
	// retrying a malformed body won't fix it.
	pendingEgressPush bool
}

type localService struct {
	cfg        Config
	store      *stateStore
	allocator  *ipAllocator
	ports      *portAllocator
	device     *machineDevice
	cubeDev    *cubeDev
	cubeRouter *cubeRouter

	mu                sync.Mutex
	states            map[string]*managedState
	tapPool           []*tapDevice
	abnormalTapPool   []*tapDevice
	quarantinedTaps   map[string]*tapDevice
	destroyFailedTaps map[string]*tapDevice
	// creating tracks sandbox IDs whose network is being created but not yet
	// committed into states. ReleaseNetwork waits on the channel so it never
	// races ahead of an in-flight creation and orphans the freshly built tap.
	creating map[string]chan struct{}

	version uint32

	// egress is the loopback admin client toward CubeEgress. nil when
	// CubeEgressAdminURL is empty (dev / test setups). The push and
	// delete sites tolerate nil; the dump endpoint exposes the current
	// state regardless of whether a client is configured.
	egress egressClient
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
	if cfg.CubeRouterEnable && cfg.CubeRouterCIDR == "" {
		allocator.ReserveLastUsable(2)
	}
	ports, err := newPortAllocator()
	if err != nil {
		return nil, err
	}
	device, err := getMachineDevice(cfg.EthName)
	if err != nil {
		return nil, err
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
	var crouter *cubeRouter
	snatIfindex := device.Index
	snatIP := device.IP
	egressSrcMac := device.Mac
	egressDstMac := device.GatewayMac
	var egressRedirectFlags uint64
	var cubeRouterIfindex uint32
	if cfg.CubeRouterEnable {
		routerSpec, err := cubeRouterSpecFromConfig(cfg)
		if err != nil {
			return nil, err
		}
		if err := ensureCubeRouterMatches(routerSpec); err != nil {
			return nil, err
		}
		crouter, err = getOrCreateCubeRouter(routerSpec, cfg.MvmMtu)
		if err != nil {
			return nil, err
		}
		if err := configureCubeRouterHostNetworking(crouter); err != nil {
			return nil, err
		}
		snatIfindex = crouter.Index
		snatIP = crouter.NATIP
		egressSrcMac = mvmMacAddr
		egressDstMac = crouter.Mac
		egressRedirectFlags = cubevs.BPFRedirectFlagIngress
		cubeRouterIfindex = uint32(crouter.Index)
	} else if err := cleanupCubeRouter(); err != nil {
		return nil, err
	}
	params := cubevs.Params{
		MVMInnerIP:          mvmInnerIP,
		MVMMacAddr:          mvmMacAddr,
		MVMGatewayIP:        mvmGatewayIP,
		Cubegw0Ifindex:      uint32(cdev.Index),
		Cubegw0IP:           cdev.IP,
		Cubegw0MacAddr:      cdev.Mac,
		EgressSrcMacAddr:    egressSrcMac,
		EgressDstMacAddr:    egressDstMac,
		EgressRedirectFlags: egressRedirectFlags,
		CubeRouterIfindex:   cubeRouterIfindex,
		NodeIfindex:         uint32(device.Index),
		NodeIP:              device.IP,
		NodeMacAddr:         device.Mac,
		NodeGatewayMacAddr:  device.GatewayMac,
	}
	if err := cubevs.Init(params); err != nil {
		return nil, err
	}
	if err := cubevs.SetSNATIPs([]*cubevs.SNATIP{{
		Ifindex: snatIfindex,
		IP:      snatIP,
	}}); err != nil {
		return nil, fmt.Errorf("set egress snat ip failed: %w", err)
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
		cubeRouter:        crouter,
		states:            make(map[string]*managedState),
		tapPool:           make([]*tapDevice, 0, cfg.TapInitNum),
		abnormalTapPool:   make([]*tapDevice, 0),
		quarantinedTaps:   make(map[string]*tapDevice),
		destroyFailedTaps: make(map[string]*tapDevice),
		creating:          make(map[string]chan struct{}),
		egress:            newEgressClientFromConfig(cfg),
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	// Pool warmup runs in the background so first-deploy startup
	// (~63ms × TapInitNum) does not block NewLocalService and trip
	// systemd's ExecStartPost timeout. EnsureNetwork transparently
	// handles an under-filled pool by creating taps on demand. See
	// code-analysis/network/11-network-agent-async-init-plan.md.
	go s.warmupTapPoolBackground()
	s.startMaintenanceLoop()
	return s, nil
}

func (s *localService) EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error) {
	CubeLog.WithContext(ctx).Infof(
		"network-agent EnsureNetwork request: sandbox_id=%s idempotency_key=%s interfaces=%d routes=%d arps=%d port_mappings=%d cube_network_config=%s persist_metadata=%v",
		req.SandboxID,
		req.IdempotencyKey,
		len(req.Interfaces),
		len(req.Routes),
		len(req.ARPNeighbors),
		len(req.PortMappings),
		formatCubeNetworkConfig(req.CubeNetworkConfig),
		req.PersistMetadata,
	)
	s.mu.Lock()
	// Fast path: already created. The common idempotent re-request returns its
	// own response clone (ensureResponse copies all slices/maps) without doing
	// any creation work.
	if existing, ok := s.states[req.SandboxID]; ok {
		resp := existing.ensureResponse()
		s.mu.Unlock()
		return resp, nil
	}
	// Deduplicate concurrent creates for the same sandbox: if one is already in
	// flight, wait on its guard channel and then return the committed state.
	if done, ok := s.creating[req.SandboxID]; ok {
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		existing, ok := s.states[req.SandboxID]
		s.mu.Unlock()
		if ok {
			return existing.ensureResponse(), nil
		}
		return nil, fmt.Errorf("concurrent network creation for sandbox %q failed", req.SandboxID)
	}
	// We are the creator. Register the guard in the SAME critical section as the
	// states/creating check, before unlocking. This closes the TOCTOU window
	// where ReleaseNetwork could observe neither states[id] nor creating[id] and
	// return a no-op release, orphaning the tap this call is about to build.
	// Different sandbox IDs still run fully in parallel: the heavy work (tap
	// creation, eBPF/cubevs map updates, state persistence) happens outside the
	// global mutex inside createState.
	if s.creating == nil {
		s.creating = make(map[string]chan struct{})
	}
	done := make(chan struct{})
	s.creating[req.SandboxID] = done
	s.mu.Unlock()

	state, createErr := s.createState(ctx, req)

	s.mu.Lock()
	delete(s.creating, req.SandboxID)
	if createErr == nil {
		s.states[state.SandboxID] = state
	}
	close(done)
	s.mu.Unlock()
	if createErr != nil {
		return nil, createErr
	}
	return state.ensureResponse(), nil
}

func (s *localService) ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error) {
	// If a creation for this sandbox is in flight, wait for it to finish before
	// looking up the state. Otherwise we could observe "not found" while a tap
	// is being built and return a no-op release, orphaning that tap. This
	// reproduces the old global-lock mutual exclusion between Ensure and Release
	// for the same sandbox without serializing unrelated sandboxes.
	s.waitForInflightCreation(req.SandboxID, req.NetworkHandle)

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

// createState builds the network for a sandbox. It does NOT hold s.mu across
// the heavy work: the global mutex is only taken briefly inside acquireTap /
// releaseAcquiredTap / cleanupConflictingTap to mutate the in-memory pools and
// collections. The expensive tap creation, eBPF/cubevs map updates and state
// persistence all run lock-free and operate on resources unique to this tap
// (distinct ifindex/IP/host-port/state file), so concurrent createState calls
// for different sandboxes proceed in parallel.
//
// ctx is currently only used for logging: the underlying netlink/eBPF calls do
// not accept a context, so a cancelled/timed-out client does not abort an
// in-flight creation. ReleaseNetwork therefore waits on the creating guard to
// avoid orphaning a tap that is still being built.
func (s *localService) createState(ctx context.Context, req *EnsureNetworkRequest) (*managedState, error) {
	if err := s.ensureHostRoute(); err != nil {
		return nil, err
	}
	requestedMappings := s.normalizePortMappings(req.PortMappings)
	tap, fromPool, err := s.acquireTap()
	if err != nil {
		return nil, err
	}
	actualMappings, err := s.configurePortMappings(tap, requestedMappings)
	if err != nil {
		if isTapCleanupError(err) {
			s.enqueueCleanupFailedTap(ctx, tap, err)
		} else {
			s.releaseAcquiredTap(tap, fromPool)
		}
		return nil, err
	}
	if err := s.registerCubeVSTap(tap.Index, tap.IP, req.SandboxID, req.CubeNetworkConfig); err != nil {
		cleanupErr := errors.Join(
			s.clearPortMappings(tap),
			s.cleanupCubeVSTap(tap.Index, tap.IP.To4()),
		)
		if cleanupErr != nil {
			s.enqueueCleanupFailedTap(ctx, tap, cleanupErr)
			return nil, errors.Join(err, markTapCleanupError(cleanupErr))
		}
		s.releaseAcquiredTap(tap, fromPool)
		return nil, err
	}
	state := &managedState{
		persistedState: persistedState{
			SandboxID:         req.SandboxID,
			NetworkHandle:     req.SandboxID,
			TapName:           tap.Name,
			TapIfIndex:        tap.Index,
			SandboxIP:         tap.IP.String(),
			Interfaces:        s.actualInterfaces(tap.Name, req.Interfaces),
			Routes:            slices.Clone(req.Routes),
			ARPNeighbors:      slices.Clone(req.ARPNeighbors),
			PortMappings:      actualMappings,
			CubeNetworkConfig: cloneCubeNetworkConfig(req.CubeNetworkConfig),
			PersistMetadata:   s.persistMetadata(req.PersistMetadata, tap.Name, tap.IP.String()),
		},
		tap:         tap,
		policyKnown: true,
	}
	if err := s.store.Save(&state.persistedState); err != nil {
		cleanupErr := errors.Join(
			s.clearPortMappings(tap),
			s.cleanupCubeVSTap(tap.Index, tap.IP.To4()),
		)
		if cleanupErr != nil {
			CubeLog.WithContext(ctx).Warnf(
				"network-agent createState rollback cleanup failed; tap will not be reused until cleanup succeeds: sandbox_id=%s ifindex=%d ip=%s err=%v",
				req.SandboxID, tap.Index, tap.IP.String(), cleanupErr,
			)
			s.enqueueCleanupFailedTap(ctx, tap, cleanupErr)
			return nil, errors.Join(err, markTapCleanupError(cleanupErr))
		}
		s.releaseAcquiredTap(tap, fromPool)
		return nil, err
	}
	// Push the L7 egress policy to CubeEgress's admin API. Best-effort:
	// the call records pendingEgressPush on transient failure so the
	// maintenance loop retries. We DO NOT unwind the tap / cubevs setup
	// if this fails — the sandbox is functional with L3/L4 enforcement
	// from cubevs, and the L7 layer is downstream of the data plane
	// init. Failing here would convert a CubeEgress hiccup into a
	// sandbox-creation failure, which is the wrong trade-off.
	s.pushEgressForState(ctx, state)
	return state, nil
}

// acquireTap obtains a tap for a new sandbox, preferring the free pool. The
// global mutex is only held for the pool dequeue and the conflict membership
// check; IP allocation and the heavy newTap syscalls run lock-free. The boolean
// return reports whether the tap came from the pool, which determines how it is
// rolled back on failure.
func (s *localService) acquireTap() (*tapDevice, bool, error) {
	s.mu.Lock()
	tap := s.dequeueTapLocked()
	s.mu.Unlock()
	if tap != nil {
		return tap, true, nil
	}
	ip, err := s.allocator.Allocate()
	if err != nil {
		return nil, false, err
	}
	if err := s.cleanupConflictingTap(ip); err != nil {
		s.allocator.Release(ip)
		return nil, false, err
	}
	tap, err = newTapFunc(ip, s.cfg.MVMMacAddr, s.cfg.MvmMtu, s.cubeDev.Index)
	if err != nil {
		s.allocator.Release(ip)
		return nil, false, err
	}
	if err := s.prepareTapForPool(tap); err != nil {
		closeTapFile(tap.File)
		_ = destroyTapFunc(tap.Index)
		s.allocator.Release(ip)
		return nil, false, err
	}
	return tap, false, nil
}

// releaseAcquiredTap rolls back a tap obtained via acquireTap after all
// stage-specific cleanup has succeeded. Pool taps are recycled back into the
// pool (their IP stays allocated for reuse); freshly created taps are destroyed
// and their IP freed.
func (s *localService) releaseAcquiredTap(tap *tapDevice, fromPool bool) {
	if fromPool {
		if err := s.prepareAndStageTapForPool(context.Background(), tap, "recycle"); err != nil {
			s.enqueuePrepareFailedTap(context.Background(), tap, err)
		}
		return
	}
	closeTapFile(tap.File)
	_ = destroyTapFunc(tap.Index)
	s.allocator.Release(tap.IP)
}

func (s *localService) cleanupCubeVSTap(ifindex int, ip net.IP) error {
	if err := cubevsDelTAPDevice(uint32(ifindex), ip); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

func (s *localService) prepareTapForPool(tap *tapDevice) error {
	if tap == nil {
		return nil
	}
	return cubevsPrepareTAPPolicy(uint32(tap.Index))
}

func (s *localService) prepareAndStageTapForPool(ctx context.Context, tap *tapDevice, reason string) error {
	if err := s.clearPortMappings(tap); err != nil {
		return markTapCleanupError(err)
	}
	if err := s.prepareTapForPool(tap); err != nil {
		return err
	}
	s.mu.Lock()
	s.stageTapForPoolLocked(tap, reason)
	s.mu.Unlock()
	return nil
}

func (s *localService) enqueuePrepareFailedTap(ctx context.Context, tap *tapDevice, err error) {
	if tap == nil {
		return
	}
	if isTapCleanupError(err) {
		s.enqueueCleanupFailedTap(ctx, tap, err)
		return
	}
	CubeLog.WithContext(ctx).Warnf(
		"network-agent tap pool preparation failed; withholding tap from reuse: name=%s ifindex=%d ip=%s err=%v",
		tap.Name, tap.Index, tap.IP, err,
	)
	s.mu.Lock()
	s.requeuePreparePoolFailureLocked(tap, err)
	s.mu.Unlock()
}

func (s *localService) enqueueCleanupFailedTap(ctx context.Context, tap *tapDevice, err error) {
	if tap == nil {
		return
	}
	CubeLog.WithContext(ctx).Errorf(
		"network-agent tap cleanup failed; withholding tap from reuse: name=%s ifindex=%d ip=%s err=%v",
		tap.Name, tap.Index, tap.IP, err,
	)
	s.mu.Lock()
	s.enqueueAbnormalLocked(tap, abnormalStageRecoverCleanup, err)
	s.mu.Unlock()
}

func (s *localService) reconcileState(ctx context.Context, state *managedState) error {
	if !state.policyKnown {
		CubeLog.WithContext(ctx).Warnf("network-agent reconcile leaves cubevs policy untouched because desired policy is unknown: sandbox_id=%s tap=%s ifindex=%d", state.SandboxID, state.TapName, state.TapIfIndex)
		return s.reconcileRecoveredState(ctx, state)
	}
	return s.reconcileStateWithCubeVSPolicy(ctx, state)
}

func (s *localService) reconcileStateWithCubeVSPolicy(ctx context.Context, state *managedState) error {
	if err := s.reconcileRuntimeState(ctx, state); err != nil {
		return err
	}
	return s.refreshCubeVSTapWithPolicy(state)
}

func (s *localService) reconcileRecoveredState(ctx context.Context, state *managedState) error {
	if err := s.reconcileRuntimeState(ctx, state); err != nil {
		return err
	}
	return s.refreshCubeVSTapForRecover(state)
}

func (s *localService) reconcileRuntimeState(ctx context.Context, state *managedState) error {
	_ = ctx
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

func (s *localService) refreshCubeVSTapForRecover(state *managedState) error {
	// Re-attach the ingress filter for recovered TAPs so the running kernel
	// always uses the currently deployed from_cube program. AttachFilter only
	// ensures per-ifindex inner maps exist; it must not replay desired policy.
	if err := cubevsAttachFilter(uint32(state.TapIfIndex)); err != nil {
		return fmt.Errorf("attach cubevs filter for tap %s(%d): %w", state.TapName, state.TapIfIndex, err)
	}
	if _, err := cubevsGetTAPDevice(uint32(state.TapIfIndex)); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			CubeLog.WithContext(context.Background()).Warnf("network-agent recover cubevs tap metadata missing, restoring metadata only: sandbox_id=%s tap=%s ifindex=%d", state.SandboxID, state.TapName, state.TapIfIndex)
			return s.upsertCubeVSTapMeta(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4(), state.SandboxID)
		}
		CubeLog.WithContext(context.Background()).Warnf("network-agent recover cubevs tap lookup failed, leaving policy untouched: tap=%s ifindex=%d err=%v", state.TapName, state.TapIfIndex, err)
	}
	return nil
}

func (s *localService) refreshCubeVSTapWithPolicy(state *managedState) error {
	if err := cubevsAttachFilter(uint32(state.TapIfIndex)); err != nil {
		return fmt.Errorf("attach cubevs filter for tap %s(%d): %w", state.TapName, state.TapIfIndex, err)
	}
	if _, err := cubevsGetTAPDevice(uint32(state.TapIfIndex)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		CubeLog.WithContext(context.Background()).Warnf("network-agent cubevs tap lookup failed before policy refresh, will upsert state: tap=%s ifindex=%d err=%v", state.TapName, state.TapIfIndex, err)
	}
	return s.upsertCubeVSTap(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4(), state.SandboxID, state.CubeNetworkConfig)
}

func (s *localService) releaseState(ctx context.Context, state *managedState) error {
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
	if err := s.clearPortMappings(state.tap); err != nil {
		return err
	}
	if err := s.cleanupCubeVSTap(state.TapIfIndex, net.ParseIP(state.SandboxIP).To4()); err != nil {
		return err
	}
	if err := s.store.Delete(state.SandboxID); err != nil {
		return err
	}
	// Best-effort: drop the L7 policy from CubeEgress before the TAP becomes
	// reusable, so a future sandbox that lands on the same IP sees a clean slate.
	// Failures here are logged at WARN, never propagated — see deleteEgressForState.
	s.deleteEgressForState(ctx, state.SandboxID, state.SandboxIP)
	if err := s.prepareAndStageTapForPool(ctx, state.tap, "recycle"); err != nil {
		s.enqueuePrepareFailedTap(ctx, state.tap, err)
	}
	return nil
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
			managed := &managedState{persistedState: *state, tap: restoredTap, policyKnown: state.CubeNetworkConfig != nil}
			managed.TapIfIndex = restoredTap.Index
			managed.TapName = restoredTap.Name
			managed.SandboxIP = restoredTap.IP.String()
			if len(restoredTap.PortMappings) > 0 {
				managed.PortMappings = append([]PortMapping(nil), restoredTap.PortMappings...)
			}
			if managed.PersistMetadata == nil {
				managed.PersistMetadata = s.persistMetadata(nil, restoredTap.Name, restoredTap.IP.String())
			}
			if err := s.reconcileRecoveredState(context.Background(), managed); err != nil {
				return err
			}
			s.states[managed.SandboxID] = managed
			recovered[managed.SandboxID] = struct{}{}
			continue
		}
		device, inCubeVS := liveCubeVSTapsByIP[restoredTap.IP.String()]
		if inCubeVS && restoredTap.InUse {
			managed := buildRecoveredState(restoredTap, &device, restoredTap.PortMappings, s.cfg)
			if err := s.reconcileRecoveredState(context.Background(), managed); err != nil {
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
			cleanupErr := errors.Join(
				s.clearPortMappings(restoredTap),
				s.cleanupCubeVSTap(restoredTap.Index, restoredTap.IP.To4()),
			)
			if cleanupErr != nil {
				s.enqueueAbnormalLocked(restoredTap, abnormalStageRecoverCleanup, cleanupErr)
				continue
			}
		}
		if err := s.prepareAndStageTapForPool(context.Background(), restoredTap, "recover"); err != nil {
			s.mu.Lock()
			s.requeuePreparePoolFailureLocked(restoredTap, err)
			s.mu.Unlock()
			continue
		}
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
			cleanupErr := errors.Join(
				s.clearPortMappings(staleTap),
				s.cleanupCubeVSTap(device.Ifindex, staleTap.IP),
			)
			if cleanupErr != nil {
				return cleanupErr
			}
		}
		if err := s.store.Delete(state.SandboxID); err != nil {
			return err
		}
	}
	return nil
}

// cleanupConflictingTap destroys a stale host tap that collides with a freshly
// allocated IP. It must be called without holding s.mu: the netlink list and the
// destroy syscall run lock-free, while the membership checks against the
// in-memory collections are performed under s.mu. The IP is already exclusively
// owned by the caller (handed out by the allocator) and tap names derive
// uniquely from the IP, so no other goroutine can re-reference this tap between
// the check and the destroy.
func (s *localService) cleanupConflictingTap(ip net.IP) error {
	taps, err := listCubeTapsFunc()
	if err != nil {
		return err
	}
	tap, ok := taps[ip.String()]
	if !ok {
		return nil
	}
	if err := s.checkTapConflict(tap, ip); err != nil {
		return err
	}
	if err := destroyTapFunc(tap.Index); err != nil {
		return fmt.Errorf("destroy stale tap %s(%d): %w", tap.Name, tap.Index, err)
	}
	return nil
}

// checkTapConflict reports an error if the given tap is still referenced by any
// in-memory collection (active states or any of the pools). It acquires s.mu
// itself for the duration of the scan, so callers must NOT already hold it
// (hence no "Locked" suffix, which would imply the opposite).
func (s *localService) checkTapConflict(tap *tapDevice, ip net.IP) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func cloneCubeNetworkConfig(in *CubeNetworkConfig) *CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
		Rules:    cloneEgressRules(in.Rules),
	}
	if in.AllowInternetAccess != nil {
		v := *in.AllowInternetAccess
		out.AllowInternetAccess = &v
	}
	return out
}

func cloneEgressRules(in []*EgressRule) []*EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*EgressRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		cp := &EgressRule{Name: r.Name}
		if r.Match != nil {
			match := *r.Match
			match.Method = append([]string(nil), r.Match.Method...)
			cp.Match = &match
		}
		if r.Action != nil {
			action := &EgressRuleAction{Allow: r.Action.Allow}
			if r.Action.Audit != nil {
				audit := *r.Action.Audit
				action.Audit = &audit
			}
			if len(r.Action.Inject) > 0 {
				action.Inject = make([]*EgressRuleInject, 0, len(r.Action.Inject))
				for _, inj := range r.Action.Inject {
					if inj == nil {
						continue
					}
					injCopy := *inj
					if inj.Format != nil {
						format := *inj.Format
						injCopy.Format = &format
					}
					action.Inject = append(action.Inject, &injCopy)
				}
			}
			cp.Action = action
		}
		out = append(out, cp)
	}
	return out
}

func formatCubeNetworkConfig(in *CubeNetworkConfig) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *in.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allowInternetAccess, in.AllowOut, in.DenyOut, len(in.Rules))
}

func (s *localService) registerCubeVSTap(ifindex int, ip net.IP, sandboxID string, cfg *CubeNetworkConfig) error {
	opts := cubeVSTapRegistration(cfg)
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent register cubevs tap: sandbox_id=%s ifindex=%d sandbox_ip=%s cube_network_config=%s allow_internet_access=%v allow_out=%v l7_allow_out=%v deny_out=%v",
		sandboxID,
		ifindex,
		ip.String(),
		formatCubeNetworkConfig(cfg),
		opts.AllowInternetAccess,
		opts.AllowOut,
		opts.L7AllowOut,
		opts.DenyOut,
	)
	return cubevsAddTAPDevice(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1), opts)
}

func (s *localService) upsertCubeVSTapMeta(ifindex int, ip net.IP, sandboxID string) error {
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent upsert cubevs tap metadata only: sandbox_id=%s ifindex=%d sandbox_ip=%s",
		sandboxID,
		ifindex,
		ip.String(),
	)
	return cubevsUpsertTAPDeviceMeta(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1))
}

func (s *localService) upsertCubeVSTap(ifindex int, ip net.IP, sandboxID string, cfg *CubeNetworkConfig) error {
	opts := cubeVSTapRegistration(cfg)
	CubeLog.WithContext(context.Background()).Infof(
		"network-agent upsert cubevs tap: sandbox_id=%s ifindex=%d sandbox_ip=%s cube_network_config=%s allow_internet_access=%v allow_out=%v l7_allow_out=%v deny_out=%v",
		sandboxID,
		ifindex,
		ip.String(),
		formatCubeNetworkConfig(cfg),
		opts.AllowInternetAccess,
		opts.AllowOut,
		opts.L7AllowOut,
		opts.DenyOut,
	)
	return cubevsUpsertTAPDevice(uint32(ifindex), ip, sandboxID, atomic.AddUint32(&s.version, 1), opts)
}

// cubeVSTapRegistration translates a CubeNetworkConfig into the cubevs MVM
// options consumed by the eBPF datapath. cubevs enforces L3/L4
// allow_internet_access / allow_out / deny_out, and it also receives network
// targets extracted from L7 rules as L7 allow targets. The complete L7 rules
// are still pushed to CubeEgress separately.
func cubeVSTapRegistration(cfg *CubeNetworkConfig) cubevs.MVMOptions {
	if cfg == nil {
		allowInternetAccess := true
		return cubevs.MVMOptions{AllowInternetAccess: &allowInternetAccess}
	}
	opts := cubevs.MVMOptions{}
	if cfg.AllowInternetAccess != nil {
		v := *cfg.AllowInternetAccess
		opts.AllowInternetAccess = &v
	} else {
		allowInternetAccess := true
		opts.AllowInternetAccess = &allowInternetAccess
	}
	if len(cfg.AllowOut) > 0 {
		allowOut := append([]string(nil), cfg.AllowOut...)
		opts.AllowOut = &allowOut
	}
	if l7AllowOut := extractL7AllowOutTargetsFromRules(cfg.Rules); len(l7AllowOut) > 0 {
		opts.L7AllowOut = &l7AllowOut
	}
	if len(cfg.DenyOut) > 0 {
		denyOut := append([]string(nil), cfg.DenyOut...)
		opts.DenyOut = &denyOut
	}
	return opts
}

func extractL7AllowOutTargetsFromRules(rules []*EgressRule) []string {
	seen := make(map[string]struct{})
	targets := make([]string, 0, len(rules))
	add := func(target string, ok bool) {
		if !ok {
			return
		}
		if _, exists := seen[target]; exists {
			return
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}

	for _, rule := range rules {
		if rule == nil || rule.Match == nil {
			continue
		}
		if rule.Match.SNI != nil {
			add(normalizeL7DomainTarget(*rule.Match.SNI))
		}
		if rule.Match.Host != nil {
			add(normalizeL7HostTarget(*rule.Match.Host))
		}
	}
	return targets
}

func normalizeL7DomainTarget(raw string) (string, bool) {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if isDottedDecimalLikeL7Target(domain) || !isValidL7DomainName(domain) {
		return "", false
	}
	return domain, true
}

func normalizeL7HostTarget(raw string) (string, bool) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		target = host
	}

	if ip := net.ParseIP(target); ip != nil {
		if ip.To4() == nil {
			return "", false
		}
		return ip.To4().String(), true
	}
	if strings.Contains(target, "/") {
		ip, ipNet, err := net.ParseCIDR(target)
		if err != nil || ip.To4() == nil {
			return "", false
		}
		return ipNet.String(), true
	}
	if isDottedDecimalLikeL7Target(target) {
		return "", false
	}
	return normalizeL7DomainTarget(target)
}

func isDottedDecimalLikeL7Target(target string) bool {
	parts := strings.Split(strings.TrimSuffix(target, "."), ".")
	if len(parts) != net.IPv4len {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

func isValidL7DomainName(domain string) bool {
	if domain == "" || len(domain) >= 254 {
		return false
	}
	if strings.Contains(domain, "*") {
		if strings.Count(domain, "*") != 1 || !strings.HasPrefix(domain, "*.") || len(domain) <= len("*.") {
			return false
		}
		domain = domain[2:]
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, ch := range label {
			isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
			if !isAlphaNum && ch != '-' {
				return false
			}
			if ch == '-' && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
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

// waitForInflightCreation blocks until any in-flight createState for the given
// sandbox ID or network handle has committed (or failed). It must be called
// without holding s.mu.
func (s *localService) waitForInflightCreation(sandboxID, networkHandle string) {
	s.mu.Lock()
	var done chan struct{}
	if sandboxID != "" {
		done = s.creating[sandboxID]
	}
	if done == nil && networkHandle != "" {
		done = s.creating[networkHandle]
	}
	s.mu.Unlock()
	if done != nil {
		<-done
	}
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

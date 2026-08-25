// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	// Function variable keeps route setup replaceable in tests without changing
	// production call sites.
	ensureRouteToCubeDevFunc = systemnet.EnsureRouteToCubeDev
)

// managedState is the runtime-owned form of a persisted sandbox network. The
// embedded persistedState is the recovery contract; the extra fields are
// process-local state used for fd handoff, policy replay, and cleanup.
type managedState struct {
	persistedState
	tap *tapDevice

	// legacyStatePath is process-local cleanup metadata for old network-agent JSON
	// files. It is never persisted as a new state-file kind.
	legacyStatePath        string
	legacyStateFingerprint string
	// migrationStateRecord is the exact new-runtime creating/deleting record paired
	// with legacyStatePath. Cleanup retires legacy first and this record second.
	migrationStateRecord *StateRecord

	// restoreBeforeCleanup asks TapCleaner to re-run TAP restore before removing
	// residue. Startup recovery uses it for no-state TAPs from smooth upgrades.
	restoreBeforeCleanup bool
}

// NetworkController coordinates the embedded runtime's durable state, host
// resources, TAP pool, and datapath adapters. Long-running kernel or HTTP work
// is intentionally kept outside s.mu; the mutex protects only in-memory maps and
// short state transitions.
type NetworkController struct {
	cfg           Config
	store         *stateStore
	allocator     *ipAllocator
	ports         *PortBinder
	device        *systemnet.HostDevice
	cubeDev       *systemnet.CubeDev
	cubeRouter    *systemnet.CubeRouter
	tapAdapter    TapDeviceAdapter
	cubevsAdapter CubeVSAdapter

	mu      sync.Mutex
	locks   *SandboxLocks
	states  map[string]*managedState
	tapPool *TapPool
	// tapFds retains one runtime-owned fd per idle pooled TAP. The fd moves to
	// the owning managedState at acquireTap and returns here on release, so the
	// GetTapFile hot path only duplicates and never pays a TUNSETIFF. Guarded
	// by mu; entries appear only via poolTapFD.
	tapFds map[string]*os.File

	version         uint32
	createStepHook  func(string)
	releaseStepHook func(string)
	cleanStepHook   func(string)

	// routeMu guards routeEnsured. The host route toward cubeDev is a node
	// invariant, so createState installs it at most once per process instead of
	// paying a netlink probe (and its RTNL queueing under bursts) per create.
	routeMu      sync.Mutex
	routeEnsured bool

	// cubeEgressAdapter is the loopback admin adapter toward CubeEgress. nil when
	// CubeEgressAdminURL is empty (dev / test setups). The push and
	// delete sites tolerate nil; the dump endpoint exposes the current
	// state regardless of whether an adapter is configured.
	cubeEgressAdapter CubeEgressAdapter
	legacyStateDir    string
}

// mustNewTapPool is used only for default dependency construction. A failure
// here means the empty pool invariant is broken, so panic is clearer than
// threading an impossible error through every caller.
func mustNewTapPool() *TapPool {
	pool, err := NewTapPool()
	if err != nil {
		panic(err)
	}
	return pool
}

// networkControllerDeps collects test seams for kernel-facing dependencies.
// Production construction fills these with real adapters in NewNetworkController.
type networkControllerDeps struct {
	store             *stateStore
	allocator         *ipAllocator
	ports             *PortBinder
	device            *systemnet.HostDevice
	cubeDev           *systemnet.CubeDev
	cubeRouter        *systemnet.CubeRouter
	tapAdapter        TapDeviceAdapter
	cubevsAdapter     CubeVSAdapter
	cubeEgressAdapter CubeEgressAdapter
	tapPool           *TapPool
	locks             *SandboxLocks
}

// newNetworkControllerFromDeps validates dependencies and builds the in-memory
// controller shell. It does not touch kernel state; callers decide when to run
// startup recovery, warmup, and maintenance loops.
func newNetworkControllerFromDeps(cfg Config, deps networkControllerDeps) (*NetworkController, error) {
	if deps.store == nil {
		return nil, fmt.Errorf("network controller requires state store")
	}
	if deps.allocator == nil {
		return nil, fmt.Errorf("network controller requires ip allocator")
	}
	if deps.ports == nil {
		return nil, fmt.Errorf("network controller requires port binder")
	}
	if deps.tapAdapter == nil {
		return nil, fmt.Errorf("network controller requires tap adapter")
	}
	if deps.cubevsAdapter == nil {
		return nil, fmt.Errorf("network controller requires cubevs adapter")
	}
	if deps.tapPool == nil {
		deps.tapPool = mustNewTapPool()
	}
	if deps.locks == nil {
		deps.locks = NewSandboxLocks()
	}
	return &NetworkController{
		cfg:               cfg,
		store:             deps.store,
		allocator:         deps.allocator,
		ports:             deps.ports,
		device:            deps.device,
		cubeDev:           deps.cubeDev,
		cubeRouter:        deps.cubeRouter,
		tapAdapter:        deps.tapAdapter,
		cubevsAdapter:     deps.cubevsAdapter,
		locks:             deps.locks,
		states:            make(map[string]*managedState),
		tapPool:           deps.tapPool,
		cubeEgressAdapter: deps.cubeEgressAdapter,
		legacyStateDir:    defaultLegacyStateDir,
	}, nil
}

// NewNetworkController initializes the production embedded runtime. The order is
// deliberate: host devices and cubevs are prepared first, then durable state and
// legacy state are reconciled with live TAP/kernel inventory.
func NewNetworkController(cfg Config) (NetworkRuntime, error) {
	deps, err := newProductionControllerDeps(cfg)
	if err != nil {
		return nil, err
	}
	controller, err := newNetworkControllerFromDeps(cfg, deps)
	if err != nil {
		return nil, err
	}
	if err := controller.startControllerRuntime(); err != nil {
		return nil, err
	}
	return controller, nil
}

func newProductionControllerDeps(cfg Config) (networkControllerDeps, error) {
	if cfg.EthName == "" {
		return networkControllerDeps{}, fmt.Errorf("network runtime requires explicit eth_name from cubelet config or flag")
	}
	store, err := newStateStore(cfg.StateDir)
	if err != nil {
		return networkControllerDeps{}, err
	}
	allocator, err := newIPAllocator(cfg.CIDR)
	if err != nil {
		return networkControllerDeps{}, err
	}
	if cfg.CubeRouterEnable && cfg.CubeRouterCIDR == "" {
		allocator.ReserveLastUsable(2)
	}
	ports, err := newPortBinder()
	if err != nil {
		return networkControllerDeps{}, err
	}
	device, err := systemnet.GetHostDevice(cfg.EthName)
	if err != nil {
		return networkControllerDeps{}, err
	}
	cubeDev, err := systemnet.GetOrCreateCubeDev(allocator.GatewayIP(), allocator.mask, cfg.MvmMtu, cfg.MvmGwMacAddr)
	if err != nil {
		return networkControllerDeps{}, err
	}
	if err := systemnet.EnsureRouteToCubeDev(cfg.CIDR, cubeDev); err != nil {
		return networkControllerDeps{}, err
	}
	cubeRouter, err := initCubeVS(cfg, device, cubeDev)
	if err != nil {
		return networkControllerDeps{}, err
	}
	return networkControllerDeps{
		store:             store,
		allocator:         allocator,
		ports:             ports,
		device:            device,
		cubeDev:           cubeDev,
		cubeRouter:        cubeRouter,
		tapAdapter:        newRealTapDeviceAdapter(),
		cubevsAdapter:     newRealCubeVSAdapter(),
		cubeEgressAdapter: newCubeEgressAdapterFromConfig(cfg),
	}, nil
}

func initCubeVS(cfg Config, device *systemnet.HostDevice, cubeDev *systemnet.CubeDev) (*systemnet.CubeRouter, error) {
	mvmInnerIP := net.ParseIP(cfg.MVMInnerIP).To4()
	mvmMacAddr, err := net.ParseMAC(cfg.MVMMacAddr)
	if err != nil {
		return nil, err
	}
	mvmGatewayIP := net.ParseIP(cfg.MvmGwDestIP).To4()
	var cubeRouter *systemnet.CubeRouter
	snatIfindex := device.Index
	snatIP := device.IP
	egressSrcMac := device.Mac
	egressDstMac := device.GatewayMac
	var egressRedirectFlags uint64
	var cubeRouterIfindex uint32
	if cfg.CubeRouterEnable {
		// In route-aware mode CubeVS redirects egress back into cube-router's
		// ingress path. SNAT and L2 destinations must therefore describe the dummy
		// router rather than the physical host uplink.
		routerSpec, err := systemnet.CubeRouterSpecFromConfig(systemnet.CubeRouterConfig{
			SandboxCIDR: cfg.CIDR,
			RouterCIDR:  cfg.CubeRouterCIDR,
			MacAddr:     cfg.CubeRouterMacAddr,
		})
		if err != nil {
			return nil, err
		}
		snatPortMin := cubeSNATPortMin()
		if err := systemnet.EnsureCubeRouterMatches(routerSpec, snatPortMin); err != nil {
			return nil, err
		}
		cubeRouter, err = systemnet.GetOrCreateCubeRouter(routerSpec, cfg.MvmMtu)
		if err != nil {
			return nil, err
		}
		if err := systemnet.ConfigureCubeRouterHostNetworking(cubeRouter, snatPortMin); err != nil {
			return nil, err
		}
		snatIfindex = cubeRouter.Index
		snatIP = cubeRouter.NATIP
		egressSrcMac = mvmMacAddr
		egressDstMac = cubeRouter.Mac
		egressRedirectFlags = cubevs.BPFRedirectFlagIngress
		cubeRouterIfindex = uint32(cubeRouter.Index)
	} else if err := systemnet.CleanupCubeRouter(cubeSNATPortMin()); err != nil {
		return nil, err
	}
	// cubevs.Params is the datapath contract shared with CubeNet/cubevs. It keeps
	// both the mvm-side gateway information and the selected egress path so eBPF
	// programs can rewrite traffic without calling back into the controller.
	params := cubevs.Params{
		MVMInnerIP:          mvmInnerIP,
		MVMMacAddr:          mvmMacAddr,
		MVMGatewayIP:        mvmGatewayIP,
		Cubegw0Ifindex:      uint32(cubeDev.Index),
		Cubegw0IP:           cubeDev.IP,
		Cubegw0MacAddr:      cubeDev.Mac,
		EgressSrcMacAddr:    egressSrcMac,
		EgressDstMacAddr:    egressDstMac,
		EgressRedirectFlags: egressRedirectFlags,
		CubeRouterIfindex:   cubeRouterIfindex,
		NodeIfindex:         uint32(device.Index),
		NodeIP:              device.IP,
		NodeIPMask:          device.IPMask,
		NodeMacAddr:         device.Mac,
		NodeGatewayMacAddr:  device.GatewayMac,
	}
	if err := loadL7MarksConfig(&params); err != nil {
		return nil, err
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
	return cubeRouter, nil
}

// l7MarksConfigPath is the install-time config shared with the
// cube-proxy-iptables-init script, so the dataplane (eBPF globals) and the
// iptables TPROXY rules stamp/match the same skb->mark values. It is a var so
// tests can point it at a temp file instead of the real /etc path.
var l7MarksConfigPath = "/etc/cubeegress/l7-marks.conf"

// loadL7MarksConfig overlays CUBE_L7_MARK_{HTTP,HTTPS,MASK} from
// l7MarksConfigPath onto params.L7Mark*. A missing file leaves the shipped
// defaults (cubevs.resolveL7Marks applies them); unset keys likewise fall
// back to defaults. Values are hex (e.g. 0xCE010000), matching the shell
// KEY=VALUE format the iptables script sources. Because the iptables script
// sources the same file as POSIX shell, hand-edited but shell-legal lines
// are tolerated here too: an "export " key prefix and a trailing
// " # comment" on the value.
func loadL7MarksConfig(params *cubevs.Params) error {
	data, err := os.ReadFile(l7MarksConfigPath) // NOCC:Path Traversal()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, "export") {
			// `export KEY=value` — without this the key would match no case
			// below and the override would be silently ignored, diverging
			// the dataplane marks from the iptables rules.
			key = strings.TrimSpace(strings.TrimPrefix(key, "export"))
		}
		// Strip a trailing ` # comment` before parsing; without this the
		// ParseUint below would fail and block controller startup.
		raw, _, _ = strings.Cut(raw, "#")
		text := strings.Trim(strings.TrimSpace(raw), `"'`)
		value, perr := strconv.ParseUint(text, 0, 32)
		if perr != nil {
			return fmt.Errorf("parse %q in %s: %w", line, l7MarksConfigPath, perr)
		}
		switch key {
		case "CUBE_L7_MARK_HTTP":
			params.L7MarkHTTP = uint32(value)
		case "CUBE_L7_MARK_HTTPS":
			params.L7MarkHTTPS = uint32(value)
		case "CUBE_L7_MARK_MASK":
			params.L7MarkMask = uint32(value)
		}
	}
	return nil
}

func startCubeVSSessionLogDrain() {
	sessionEvents := cubevs.StartSessionReaper()
	go func() {
		// StartSessionReaper owns its own lifecycle; the controller only drains the
		// event channel so warnings remain visible in Cubelet logs.
		logger := CubeLog.WithContext(context.Background())
		for event := range sessionEvents {
			if event.Error != nil {
				logger.Warnf("cubevs session reaper: %v: %s", event.Error, event.Message)
			}
		}
	}()
}

func (s *NetworkController) startControllerRuntime() error {
	if err := s.recover(); err != nil {
		return err
	}
	// Stale HashOfMaps cleanup is part of startup reconciliation. Complete it
	// before starting the reaper, background pool creation, or returning the
	// controller to request-serving code so it cannot race a new TAP lifecycle.
	s.runStaleNetPolicyMapGC()
	startCubeVSSessionLogDrain()

	// Pool warmup runs in the background so first-deploy startup
	// (~63ms × TapInitNum) does not block NewNetworkController and trip
	// systemd's ExecStartPost timeout. EnsureNetwork transparently
	// handles an under-filled pool by creating taps on demand. See
	// code-analysis/network/11-network runtime-async-init-plan.md.
	go s.warmupTapPoolBackground()
	s.startMaintenanceLoop()
	return nil
}

// ensurePrecheckTimings captures the pre-create phases in EnsureNetwork before
// createState is invoked. Zero-valued fields mean the phase did not complete
// (early return on error or idempotent fast path).
type ensurePrecheckTimings struct {
	lockWait    time.Duration
	inmemLookup time.Duration
	existsCheck time.Duration
	legacyCheck time.Duration
	createState time.Duration
}

// slowStageLogThreshold gates the per-stage timing logs (ensure precheck,
// create stages, register cubevs tap stages, tap fd handoff): they are
// emitted at Warn level only when the operation fails or exceeds this
// duration, keeping normal-path log volume near zero while preserving slow
// samples for latency analysis at any log level.
const slowStageLogThreshold = time.Millisecond

// createStageTimings captures per-stage durations for createState. Zero-valued
// fields indicate the stage did not run because an earlier step failed and
// returned early.
type createStageTimings struct {
	ensureRoute       time.Duration
	acquireTap        time.Duration
	reservePorts      time.Duration
	writeTmp          time.Duration
	commitCreating    time.Duration
	applyPortMappings time.Duration
	registerCubeVS    time.Duration
	pushEgress        time.Duration
	commitSuccess     time.Duration
	commitActive      time.Duration
}

// EnsureNetwork is the idempotent public create entry point. It serializes only
// requests for the same sandbox, then lets different sandboxes perform heavy TAP
// creation and datapath programming concurrently.
func (s *NetworkController) EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (resp *EnsureNetworkResponse, err error) {
	if req == nil {
		return nil, fmt.Errorf("ensure network request is nil")
	}
	totalStart := time.Now()
	stageStart := totalStart
	var pre ensurePrecheckTimings
	idempotent := false
	defer func() {
		if total := time.Since(totalStart); err != nil || total >= slowStageLogThreshold {
			CubeLog.WithContext(ctx).Warnf(
				"network runtime ensure precheck: sandbox_id=%s total=%s success=%t idempotent=%t lock_wait=%s inmem_lookup=%s exists_check=%s legacy_check=%s create_state=%s",
				req.SandboxID, total, err == nil, idempotent,
				pre.lockWait, pre.inmemLookup, pre.existsCheck, pre.legacyCheck, pre.createState,
			)
		}
	}()
	unlock := func() {}
	if s.locks != nil {
		unlock = s.locks.Lock(req.SandboxID)
	}
	defer unlock()
	pre.lockWait = time.Since(stageStart)
	stageStart = time.Now()

	CubeLog.WithContext(ctx).Infof(
		"network runtime EnsureNetwork request: sandbox_id=%s idempotency_key=%s interfaces=%d routes=%d arps=%d port_mappings=%d cube_network_config=%s persist_metadata=%v",
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
	if existing, ok := s.states[req.SandboxID]; ok {
		resp = existing.ensureResponse()
		s.mu.Unlock()
		idempotent = true
		return resp, nil
	}
	s.mu.Unlock()
	pre.inmemLookup = time.Since(stageStart)
	stageStart = time.Now()
	for _, kind := range []StateFileKind{StateFileCreating, StateFileSuccess, StateFileDeleting} {
		if s.store.Exists(req.SandboxID, kind) {
			return nil, fmt.Errorf("network lifecycle %s already exists for sandbox %q; recovery or cleanup is still pending", kind, req.SandboxID)
		}
	}
	pre.existsCheck = time.Since(stageStart)
	stageStart = time.Now()
	if pending, err := s.hasPendingLegacyState(req.SandboxID); err != nil {
		return nil, fmt.Errorf("check legacy network lifecycle for sandbox %q: %w", req.SandboxID, err)
	} else if pending {
		return nil, fmt.Errorf("legacy network lifecycle already exists for sandbox %q; recovery or cleanup is still pending", req.SandboxID)
	}
	pre.legacyCheck = time.Since(stageStart)
	stageStart = time.Now()

	state, createErr := s.createState(ctx, req)
	if createErr != nil {
		return nil, createErr
	}
	s.mu.Lock()
	s.states[state.SandboxID] = state
	s.mu.Unlock()
	pre.createState = time.Since(stageStart)
	return state.ensureResponse(), nil
}

// UpdateNetworkPolicy replaces the egress policy of a running sandbox.
//
// Step order is the contract, not an implementation detail:
//
//  1. CubeEgress before CubeVS. Whichever way the rule set moves the transient
//     state errs towards more interception: a new rule is installed before
//     anything is steered at it, and a removed rule is gone before the datapath
//     stops steering, so leftover traffic meets the proxy's default deny.
//  2. CubeVS second, which also bumps the policy generation and so is the point
//     where established flows start being re-evaluated.
//  3. Persist last. A crash before this replays the previous policy on restart
//     — the safe direction, since the caller was never told it succeeded.
//
// There is no rollback. The CubeVS diff is computed from the live maps, so
// replaying the same request converges.
func (s *NetworkController) UpdateNetworkPolicy(ctx context.Context, req *UpdateNetworkPolicyRequest) error {
	if req == nil || req.SandboxID == "" {
		return fmt.Errorf("sandboxID is required")
	}
	unlock := func() {}
	if s.locks != nil {
		unlock = s.locks.Lock(req.SandboxID)
	}
	defer unlock()

	s.mu.Lock()
	state, ok := s.states[req.SandboxID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: sandbox %q", ErrNetworkNotActive, req.SandboxID)
	}

	CubeLog.WithContext(ctx).Infof(
		"network runtime UpdateNetworkPolicy request: sandbox_id=%s sandbox_ip=%s cube_network_config=%s",
		req.SandboxID, state.SandboxIP, formatCubeNetworkConfig(req.CubeNetworkConfig),
	)

	// A sandbox created before the runtime recorded its resolvers has them in
	// its installed AllowOut but nowhere we can identify them, so fall back to
	// the caller's list rather than silently revoking DNS. Persisting it below
	// means each sandbox needs the fallback at most once.
	resolverCIDRs := state.DNSAllowOutCIDRs
	if len(resolverCIDRs) == 0 {
		resolverCIDRs = req.DNSAllowOutCIDRs
	}
	cfg := withDNSResolverAllowOut(cloneCubeNetworkConfig(req.CubeNetworkConfig), resolverCIDRs)
	if err := s.syncEgressPolicy(ctx, state, cfg); err != nil {
		return fmt.Errorf("sync CubeEgress policy for sandbox %s: %w", req.SandboxID, err)
	}
	if err := s.updateCubeVSTapPolicy(state.TapIfIndex, req.SandboxID, cfg); err != nil {
		return fmt.Errorf("update CubeVS policy for sandbox %s: %w", req.SandboxID, err)
	}

	s.mu.Lock()
	state.CubeNetworkConfig = cfg
	state.DNSAllowOutCIDRs = resolverCIDRs
	s.mu.Unlock()

	if err := s.store.RewriteSuccess(&state.persistedState); err != nil {
		return fmt.Errorf("persist network policy for sandbox %s: %w", req.SandboxID, err)
	}
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
// not accept a context. The per-sandbox lock held by EnsureNetwork serializes
// this entire operation with ReleaseNetwork for the same sandbox.
func (s *NetworkController) createState(ctx context.Context, req *EnsureNetworkRequest) (state *managedState, err error) {
	totalStart := time.Now()
	stageStart := totalStart
	var t createStageTimings
	defer func() {
		if total := time.Since(totalStart); err != nil || total >= slowStageLogThreshold {
			CubeLog.WithContext(ctx).Warnf(
				"network runtime create stages: sandbox_id=%s total=%s success=%t port_mappings=%d ensure_route=%s acquire_tap=%s reserve_ports=%s write_tmp=%s commit_creating=%s apply_port_mappings=%s register_cubevs_tap=%s push_egress=%s commit_success=%s commit_active=%s",
				req.SandboxID, total, err == nil, len(req.PortMappings),
				t.ensureRoute, t.acquireTap, t.reservePorts, t.writeTmp, t.commitCreating,
				t.applyPortMappings, t.registerCubeVS, t.pushEgress,
				t.commitSuccess, t.commitActive,
			)
		}
	}()
	if err := s.ensureHostRoute(); err != nil {
		return nil, err
	}
	t.ensureRoute = time.Since(stageStart)
	stageStart = time.Now()
	requestedMappings := s.normalizePortMappings(req.PortMappings)
	tap, entry, err := s.acquireTap(req.SandboxID)
	if err != nil {
		return nil, err
	}
	t.acquireTap = time.Since(stageStart)
	stageStart = time.Now()
	actualMappings, err := s.reservePortMappings(req.SandboxID, tap, requestedMappings)
	if err != nil {
		s.releaseAcquiredTap(req.SandboxID, tap, entry)
		return nil, err
	}
	t.reservePorts = time.Since(stageStart)
	stageStart = time.Now()
	state = &managedState{
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
			DNSAllowOutCIDRs:  slices.Clone(req.DNSAllowOutCIDRs),
			PersistMetadata:   s.persistMetadata(req.PersistMetadata, tap.Name, tap.IP.String()),
		},
		tap: tap,
	}
	if err := s.store.WriteTmp(&state.persistedState); err != nil {
		s.releasePortOwnership(req.SandboxID)
		s.releaseAcquiredTap(req.SandboxID, tap, entry)
		return nil, err
	}
	t.writeTmp = time.Since(stageStart)
	stageStart = time.Now()
	// Persist the create intent before programming kernel-visible state. After
	// this rename, startup recovery has enough information to finish cleanup if
	// the process exits mid-create.
	if err := s.store.CommitCreating(state.SandboxID); err != nil {
		current, inspectErr := s.store.LoadAny(state.SandboxID)
		if inspectErr == nil && current.Kind == StateFileCreating {
			// Rename may have completed before directory fsync reported failure.
			// The durable creating intent now owns rollback; never return its TAP
			// to Ready outside the normal cleaner.
			s.cleanupCreateFailure(ctx, state, err)
			return nil, err
		}
		if inspectErr == nil && current.Kind == StateFileTmp {
			deleted, deleteErr := s.store.DeleteRecordIfCurrent(current)
			s.releasePortOwnership(req.SandboxID)
			s.releaseAcquiredTap(req.SandboxID, tap, entry)
			if deleteErr != nil || !deleted {
				return nil, errors.Join(err, deleteErr, fmt.Errorf("could not retire failed create tmp state: path=%s", current.Path))
			}
			return nil, err
		}
		if errors.Is(inspectErr, os.ErrNotExist) {
			s.releasePortOwnership(req.SandboxID)
			s.releaseAcquiredTap(req.SandboxID, tap, entry)
			return nil, err
		}
		if inspectErr != nil {
			s.closeRuntimeTapOwnership(state)
			return nil, errors.Join(err, fmt.Errorf("inspect state after creating commit failure: %w", inspectErr))
		}
		s.closeRuntimeTapOwnership(state)
		return nil, fmt.Errorf("create commit failed with unexpected durable state %s at %s: %w", current.Kind, current.Path, err)
	}
	t.commitCreating = time.Since(stageStart)
	stageStart = time.Now()
	s.recordCreateStep("creating")
	if err := s.applyPortMappings(req.SandboxID, tap); err != nil {
		s.cleanupCreateFailure(ctx, state, err)
		return nil, err
	}
	t.applyPortMappings = time.Since(stageStart)
	stageStart = time.Now()
	if err := s.registerCubeVSTap(tap.Index, tap.IP, req.SandboxID, req.CubeNetworkConfig); err != nil {
		s.cleanupCreateFailure(ctx, state, err)
		return nil, err
	}
	t.registerCubeVS = time.Since(stageStart)
	stageStart = time.Now()
	if err := s.pushEgressForState(ctx, state); err != nil {
		s.cleanupCreateFailure(ctx, state, err)
		return nil, err
	}
	t.pushEgress = time.Since(stageStart)
	stageStart = time.Now()
	// Success is the durable point of no return for recovery: a restarted runtime
	// treats this sandbox as active unless ReleaseNetwork later marks it deleting.
	// Host ports are already owned from Reserve; create rollback and cleanup both
	// release them via ReleaseOwnership.
	s.recordCreateStep("pre_success")
	if err := s.store.CommitSuccess(state.SandboxID); err != nil {
		current, inspectErr := s.store.LoadAny(state.SandboxID)
		if inspectErr == nil {
			switch current.Kind {
			case StateFileSuccess:
				s.closeRuntimeTapOwnership(state)
				return nil, errors.Join(
					ErrEnsureNetworkCommitted,
					fmt.Errorf("network success may already be durable for sandbox %s; refusing in-transaction create rollback: %w", state.SandboxID, err),
				)
			case StateFileCreating:
				s.cleanupCreateFailure(ctx, state, err)
				return nil, err
			default:
				s.closeRuntimeTapOwnership(state)
				return nil, fmt.Errorf("success commit failed with unexpected durable state %s at %s: %w", current.Kind, current.Path, err)
			}
		}
		if errors.Is(inspectErr, os.ErrNotExist) {
			s.cleanupCreateFailure(ctx, state, err)
			return nil, err
		}
		s.closeRuntimeTapOwnership(state)
		return nil, errors.Join(err, fmt.Errorf("inspect state after success commit failure: %w", inspectErr))
	}
	t.commitSuccess = time.Since(stageStart)
	stageStart = time.Now()
	s.recordCreateStep("success")
	// Publish the TAP as Active only after the success state is durable. This
	// prevents an fd handoff for a sandbox that recovery would not consider live.
	if err := s.tapPool.CommitActive(entry, req.SandboxID); err != nil {
		s.closeRuntimeTapOwnership(state)
		return nil, errors.Join(
			ErrEnsureNetworkCommitted,
			fmt.Errorf("network success committed for sandbox %s but Active publication failed; refusing in-transaction create rollback: %w", state.SandboxID, err),
		)
	}
	t.commitActive = time.Since(stageStart)
	s.recordCreateStep("active")
	// A newly-created TAP already has the one runtime-owned fd needed for shim
	// handoff. Keep it as the Active state's cache and hand out only duplicates.
	// TAPs acquired from Ready, and Active TAPs recovered after restart, still
	// open this cache lazily because their idle/recovered fd is intentionally nil.
	return state, nil
}

// recordCreateStep is a test hook for crash-point ordering assertions.
func (s *NetworkController) recordCreateStep(step string) {
	if s.createStepHook != nil {
		s.createStepHook(step)
	}
}

// cleanupCreateFailure moves a partially-created TAP into Cleaning and performs
// one bounded cleanup attempt. The durable creating state remains when cleanup
// fails, and maintenance retries it on its next periodic pass. This helper is
// intentionally pre-success only: committed success is never rolled back here.
func (s *NetworkController) cleanupCreateFailure(ctx context.Context, state *managedState, cause error) {
	if state == nil {
		return
	}
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
	if cause != nil {
		state.tap.LastError = cause.Error()
		state.tap.LastStage = "create_failure"
	}
	if err := s.beginTapCleanup(state.tap, state.SandboxID); err != nil {
		CubeLog.WithContext(ctx).Errorf("network runtime create failed but tap could not enter Cleaning: sandbox_id=%s tap=%s err=%v", state.SandboxID, state.TapName, err)
		s.closeRuntimeTapOwnership(state)
		return
	}
	s.closeRuntimeTapOwnership(state)
	if cleanupErr := s.cleanupTapOnce(state, StateFileCreating, "create_failure"); cleanupErr != nil {
		CubeLog.WithContext(ctx).Warnf("network runtime create failed after %s state; durable cleanup remains pending: sandbox_id=%s tap=%s ifindex=%d ip=%s create_err=%v cleanup_err=%v",
			StateFileCreating, state.SandboxID, state.TapName, state.TapIfIndex, state.SandboxIP, cause, cleanupErr)
		return
	}
	CubeLog.WithContext(ctx).Warnf("network runtime create failed after %s state and synchronous cleanup completed: sandbox_id=%s tap=%s ifindex=%d ip=%s err=%v",
		StateFileCreating, state.SandboxID, state.TapName, state.TapIfIndex, state.SandboxIP, cause)
}

// acquireTap obtains a Ready tap for a new sandbox through TapPool. The entry
// remains Ready but owner-reserved until success is committed; Active is only
// published after creating -> success.
func (s *NetworkController) acquireTap(owner string) (*tapDevice, *TapPoolEntry, error) {
	entry, err := s.tapPool.Acquire(owner)
	if err == nil {
		tap, err := tapDeviceFromEntry(entry)
		if err != nil {
			return nil, nil, err
		}
		// Move the retained idle fd into Active ownership so the first
		// GetTapFile handoff is a cache hit. Nil after a runtime restart;
		// GetTapFile then falls back to a lazy open.
		tap.File = s.takePooledTapFD(entry.TapName)
		return tap, entry, nil
	}
	ip, err := s.allocator.Allocate()
	if err != nil {
		return nil, nil, err
	}
	if err := s.cleanupConflictingTap(ip); err != nil {
		s.allocator.Release(ip)
		return nil, nil, err
	}
	tap, err := s.tapAdapter.Create(ip, s.cfg.MVMMacAddr, s.cfg.MvmMtu, s.cubeDev.Index)
	if err != nil {
		s.allocator.Release(ip)
		return nil, nil, err
	}
	if err := s.resetTapPolicyForPool(tap); err != nil {
		s.tapAdapter.Close(tap.File)
		_ = s.tapAdapter.Destroy(tap.Index)
		s.allocator.Release(ip)
		return nil, nil, err
	}
	entry, err = tapPoolEntryFromDevice(tap, "", TapPoolReady)
	if err != nil {
		s.tapAdapter.Close(tap.File)
		_ = s.tapAdapter.Destroy(tap.Index)
		s.allocator.Release(ip)
		return nil, nil, err
	}
	if err := s.tapPool.AddReserved(entry, owner); err != nil {
		s.tapAdapter.Close(tap.File)
		_ = s.tapAdapter.Destroy(tap.Index)
		s.allocator.Release(ip)
		return nil, nil, err
	}
	return tap, entry, nil
}

// releaseAcquiredTap rolls back a tap obtained via acquireTap before the state
// file reaches creating. At this point no recoverable state file exists and no
// CubeVS/CubeEgress/port binding side effects are allowed, so rollback only
// resets local runtime fields and then releases the in-memory owner reservation.
func (s *NetworkController) releaseAcquiredTap(owner string, tap *tapDevice, entry *TapPoolEntry) {
	s.resetTapRuntimeFieldsForPool(tap)
	if entry != nil {
		if err := s.tapPool.ReleaseReservation(entry, owner); err != nil {
			CubeLog.WithContext(context.Background()).Warnf("network runtime could not release pre-create TAP reservation; keeping TAP in Cleaning: sandbox_id=%s tap=%s err=%v", owner, tap.Name, err)
			if _, beginErr := s.tapPool.BeginCleanupByName(tap.Name, owner); beginErr == nil {
				state := &managedState{persistedState: persistedState{
					SandboxID:  owner,
					TapName:    tap.Name,
					TapIfIndex: tap.Index,
					SandboxIP:  tap.IP.String(),
				}, tap: tap, restoreBeforeCleanup: true}
				_ = s.cleanupTapOnce(state, "", "pre_create_reservation_rollback")
			}
			return
		}
	}
}

// ReleaseNetwork is the idempotent public release entry point. The sandbox lock
// serializes it with EnsureNetwork. Once the deleting rename commits, the state
// is never published Active again: cleanup runs once synchronously and durable
// state is left for maintenance when that attempt fails.
func (s *NetworkController) ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("release network request is nil")
	}
	lockKey := req.SandboxID
	if lockKey == "" {
		lockKey = req.NetworkHandle
	}
	unlock := func() {}
	if s.locks != nil {
		unlock = s.locks.Lock(lockKey)
	}
	defer unlock()

	s.mu.Lock()
	state, ok := s.lookupStateLocked(req.SandboxID, req.NetworkHandle)
	s.mu.Unlock()
	if !ok {
		sandboxID := req.SandboxID
		if sandboxID == "" {
			sandboxID = req.NetworkHandle
		}
		if sandboxID == "" {
			return &ReleaseNetworkResponse{Released: true, PersistMetadata: req.PersistMetadata}, nil
		}
		record, err := s.store.LoadAny(sandboxID)
		if err == nil {
			metadata := cloneStringMap(record.State.PersistMetadata)
			if cleanupErr := s.releaseDurableRecord(ctx, record); cleanupErr != nil {
				return &ReleaseNetworkResponse{Released: true, PersistMetadata: metadata}, cleanupErr
			}
			return &ReleaseNetworkResponse{Released: true, PersistMetadata: metadata}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return &ReleaseNetworkResponse{Released: false, PersistMetadata: req.PersistMetadata},
				fmt.Errorf("load pending network lifecycle for sandbox %s: %w", sandboxID, err)
		}
		return &ReleaseNetworkResponse{Released: true, PersistMetadata: req.PersistMetadata}, nil
	}

	if err := s.releaseState(ctx, state); err != nil {
		return &ReleaseNetworkResponse{
			// Once success is gone, the delete intent either remains durable or
			// cleanup has already completed. A directory-fsync error after rename
			// must not report Released=false after successful cleanup.
			Released:        !s.store.Exists(state.SandboxID, StateFileSuccess),
			PersistMetadata: state.PersistMetadata,
		}, err
	}
	return &ReleaseNetworkResponse{
		Released:        true,
		PersistMetadata: state.PersistMetadata,
	}, nil
}

// releaseDurableRecord handles an explicit release after process-local Active
// state has already disappeared (for example, create rollback cleanup failed).
// The sandbox lock held by ReleaseNetwork serializes this attempt with Ensure
// and maintenance.
func (s *NetworkController) releaseDurableRecord(ctx context.Context, record *StateRecord) error {
	if record == nil || record.State == nil {
		return nil
	}
	var transitionErr error
	switch record.Kind {
	case StateFileTmp:
		deleted, deleteErr := s.store.DeleteRecordIfCurrent(record)
		if deleteErr != nil {
			return deleteErr
		}
		if !deleted {
			return fmt.Errorf("tmp network lifecycle changed before release: path=%s", record.Path)
		}
		return nil
	case StateFileSuccess:
		if err := s.store.MarkDeleting(record.State.SandboxID); err != nil {
			current, inspectErr := s.store.LoadAny(record.State.SandboxID)
			if inspectErr != nil {
				return errors.Join(err, fmt.Errorf("inspect durable state after deleting commit failure: %w", inspectErr))
			}
			switch current.Kind {
			case StateFileSuccess:
				return err
			case StateFileDeleting:
				transitionErr = err
				record = current
			default:
				return fmt.Errorf("deleting commit failed with unexpected durable state %s at %s: %w", current.Kind, current.Path, err)
			}
		} else {
			var err error
			record, err = s.store.LoadAny(record.State.SandboxID)
			if err != nil {
				return err
			}
		}
	case StateFileCreating, StateFileDeleting:
		// Both are already durable cleanup intents.
	default:
		return fmt.Errorf("unsupported durable network lifecycle %q for sandbox %s", record.Kind, record.State.SandboxID)
	}

	var tap *tapDevice
	if entry, ok := s.tapPool.GetByName(record.State.TapName); ok {
		var tapErr error
		tap, tapErr = tapDeviceFromEntry(entry)
		if tapErr != nil {
			return tapErr
		}
	} else {
		liveTaps, listErr := s.tapAdapter.List()
		if listErr != nil {
			return fmt.Errorf("list TAPs for pending release of sandbox %s: %w", record.State.SandboxID, listErr)
		}
		tap = liveTapForState(record.State, liveTaps)
	}
	if tap == nil {
		if err := s.cleanupStateOnlyOnce(record, "explicit_release_state_only"); err != nil {
			return errors.Join(transitionErr, fmt.Errorf("network release committed for sandbox %s but state-only cleanup remains pending: %w", record.State.SandboxID, err))
		}
		return transitionErr
	}
	state := managedStateForCleanupRecord(record, tap)
	if err := s.beginTapCleanup(tap, state.SandboxID); err != nil {
		return errors.Join(transitionErr, fmt.Errorf("network release committed for sandbox %s but TAP could not enter Cleaning: %w", state.SandboxID, err))
	}
	s.closeRuntimeTapOwnership(state)
	if err := s.cleanupTapOnce(state, record.Kind, "explicit_release_pending_lifecycle"); err != nil {
		CubeLog.WithContext(ctx).Warnf(
			"network runtime explicit release found pending lifecycle but cleanup remains pending: sandbox_id=%s kind=%s tap=%s err=%v",
			state.SandboxID, record.Kind, state.TapName, err,
		)
		return errors.Join(transitionErr, fmt.Errorf("network release committed for sandbox %s but cleanup remains pending: %w", state.SandboxID, err))
	}
	return transitionErr
}

// ListTaps returns a diagnostic view of every TapPool entry, including entries
// not associated with active managedState.
func (s *NetworkController) ListTaps(ctx context.Context, req *ListTapsRequest) (*ListTapsResponse, error) {
	_ = ctx
	_ = req
	statesByTapName := map[string]*managedState{}
	s.mu.Lock()
	for _, state := range s.states {
		statesByTapName[state.TapName] = state
	}
	s.mu.Unlock()

	taps := make([]TapState, 0)
	stateCounts := map[string]int{}
	for _, entry := range s.tapPool.Entries() {
		if entry == nil {
			continue
		}
		stateCounts[string(entry.State)]++
		tap := TapState{
			TapName:        entry.TapName,
			TapIfIndex:     int32(entry.TapIfIndex),
			SandboxIP:      entry.SandboxIP.String(),
			State:          string(entry.State),
			OwnerSandboxID: entry.OwnerSandboxID,
			RetryCount:     entry.RetryCount,
			LastError:      entry.LastError,
		}
		if state := statesByTapName[entry.TapName]; state != nil {
			tap.SandboxID = state.SandboxID
			tap.NetworkHandle = state.NetworkHandle
			tap.PortMappings = slices.Clone(state.PortMappings)
		} else if entry.OwnerSandboxID != "" {
			tap.SandboxID = entry.OwnerSandboxID
			tap.NetworkHandle = entry.OwnerSandboxID
		}
		taps = append(taps, tap)
	}
	slices.SortFunc(taps, func(a, b TapState) int {
		if a.State != b.State {
			return strings.Compare(a.State, b.State)
		}
		return strings.Compare(a.TapName, b.TapName)
	})
	return &ListTapsResponse{Taps: taps, StateCounts: stateCounts}, nil
}

// Health currently reports only process liveness. Kernel/datapath deep checks
// are intentionally not performed on the hot health endpoint.
func (s *NetworkController) Health(ctx context.Context) error {
	_ = ctx
	return nil
}

// cleanupCubeVSTapState removes sandbox-specific CubeVS policy residue and TAP
// metadata, then verifies the ifindex no longer has sandbox metadata before
// cleanup may return the TAP to Ready.
func (s *NetworkController) cleanupCubeVSTapState(ifindex int, ip net.IP) error {
	if err := s.cubevsAdapter.CleanupTAPPolicy(uint32(ifindex)); err != nil {
		return err
	}
	return s.cleanupCubeVSTapMetadata(ifindex, ip)
}

func (s *NetworkController) cleanupCubeVSTapMetadata(ifindex int, ip net.IP) error {
	if err := s.cubevsAdapter.DeleteTAPDeviceMetadata(uint32(ifindex), ip); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	if _, err := s.cubevsAdapter.GetTAPDevice(uint32(ifindex)); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		return fmt.Errorf("verify cubevs tap metadata absent for ifindex %d: %w", ifindex, err)
	}
	return fmt.Errorf("cubevs tap metadata still exists after cleanup for ifindex %d", ifindex)
}

// recordReleaseStep is a test hook for release crash-point ordering assertions.
func (s *NetworkController) recordReleaseStep(step string) {
	if s.releaseStepHook != nil {
		s.releaseStepHook(step)
	}
}

// releaseState commits the deleting intent, makes fd handoff impossible, removes
// the state from the Active map, and performs one bounded cleanup attempt.
func (s *NetworkController) releaseState(ctx context.Context, state *managedState) error {
	var transitionErr error
	if err := s.store.MarkDeleting(state.SandboxID); err != nil {
		current, inspectErr := s.store.LoadAny(state.SandboxID)
		if inspectErr != nil {
			return errors.Join(err, fmt.Errorf("inspect state after deleting commit failure: %w", inspectErr))
		}
		switch current.Kind {
		case StateFileSuccess:
			return err
		case StateFileDeleting:
			// Rename may have completed before directory fsync reported failure.
			// Continue the durable cleanup handoff, but preserve the durability
			// error in the result so the caller knows the commit was not confirmed.
			transitionErr = err
		default:
			return fmt.Errorf("deleting commit failed with unexpected durable state %s at %s: %w", current.Kind, current.Path, err)
		}
	}
	s.recordReleaseStep("deleting")
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
	if err := s.beginTapCleanup(state.tap, state.SandboxID); err != nil {
		s.closeRuntimeTapOwnership(state)
		s.mu.Lock()
		delete(s.states, state.SandboxID)
		s.mu.Unlock()
		return errors.Join(transitionErr, fmt.Errorf("network release committed deleting state but could not enter Cleaning for sandbox %s: %w", state.SandboxID, err))
	}
	s.recordReleaseStep("cleaning")
	s.closeRuntimeTapOwnership(state)
	s.recordReleaseStep("fd_close")
	s.mu.Lock()
	delete(s.states, state.SandboxID)
	s.mu.Unlock()
	s.recordReleaseStep("active_removed")
	if err := s.cleanupTapOnce(state, StateFileDeleting, "release"); err != nil {
		CubeLog.WithContext(ctx).Warnf("network runtime release committed but synchronous cleanup remains pending: sandbox_id=%s tap=%s ifindex=%d ip=%s err=%v",
			state.SandboxID, state.TapName, state.TapIfIndex, state.SandboxIP, err)
		return errors.Join(transitionErr, fmt.Errorf("network release committed for sandbox %s but cleanup remains pending: %w", state.SandboxID, err))
	}
	s.recordReleaseStep("cleaned")
	CubeLog.WithContext(ctx).Infof("network runtime release completed: sandbox_id=%s tap=%s ifindex=%d ip=%s", state.SandboxID, state.TapName, state.TapIfIndex, state.SandboxIP)
	return transitionErr
}

// SandboxLocks provides per-sandbox serialization for public Ensure/Release
// operations. It preserves the old same-sandbox mutual exclusion without
// reintroducing a single global lock for unrelated sandbox IDs.
type SandboxLocks struct {
	mu    sync.Mutex
	locks map[string]*sandboxLockRef
}

type sandboxLockRef struct {
	mu   sync.Mutex
	refs int
}

// NewSandboxLocks creates an empty per-key lock registry.
func NewSandboxLocks() *SandboxLocks {
	return &SandboxLocks{locks: make(map[string]*sandboxLockRef)}
}

// Lock returns an unlock function for key. The registry uses reference counts so
// idle per-sandbox mutexes are removed after the last waiter leaves.
func (l *SandboxLocks) Lock(key string) func() {
	if key == "" {
		key = "__default__"
	}
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sandboxLockRef)
	}
	ref := l.locks[key]
	if ref == nil {
		ref = &sandboxLockRef{}
		l.locks[key] = ref
	}
	ref.refs++
	l.mu.Unlock()

	ref.mu.Lock()
	return func() {
		ref.mu.Unlock()
		l.mu.Lock()
		ref.refs--
		if ref.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

// normalizePortMappings removes invalid zero-container-port entries, fills
// defaults, and keeps only the last mapping per container port. Sorting makes
// persisted state deterministic across retries.
func (s *NetworkController) normalizePortMappings(req []PortMapping) []PortMapping {
	byContainerPort := make(map[int32]PortMapping)
	for _, mapping := range req {
		if mapping.ContainerPort == 0 {
			continue
		}
		if mapping.HostIP == "" {
			mapping.HostIP = s.cfg.HostPortBindIP
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

// actualInterfaces returns the interface contract visible to Cubelet. The first
// interface is always bound to the host TAP name because that is the concrete
// device created by the runtime.
func (s *NetworkController) actualInterfaces(tapName string, req []Interface) []Interface {
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

// persistMetadata merges caller metadata with runtime-generated fields that are
// useful for release idempotency and external diagnostics.
func (s *NetworkController) persistMetadata(base map[string]string, tapName string, sandboxIP string) map[string]string {
	metadata := cloneStringMap(base)
	metadata["sandbox_ip"] = sandboxIP
	metadata["host_tap_name"] = tapName
	metadata["mvm_inner_ip"] = s.cfg.MVMInnerIP
	metadata["gateway_ip"] = s.cfg.MvmGwDestIP
	return metadata
}

// ensureHostRoute reasserts the route to the sandbox CIDR through cube-dev.
// ensureHostRoute installs the node-invariant route toward cubeDev once per
// process; subsequent creates skip the netlink probe entirely. A failure is
// not latched: the next create retries the installation.
func (s *NetworkController) ensureHostRoute() error {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routeEnsured {
		return nil
	}
	if err := ensureRouteToCubeDevFunc(s.cfg.CIDR, s.cubeDev); err != nil {
		return err
	}
	s.routeEnsured = true
	return nil
}

// lookupStateLocked finds a managed state by SandboxID or NetworkHandle. s.mu
// must be held by the caller.
func (s *NetworkController) lookupStateLocked(sandboxID, networkHandle string) (*managedState, bool) {
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

// ensureResponse returns a defensive-copy response so callers cannot mutate the
// controller's managed state through returned slices or maps.
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

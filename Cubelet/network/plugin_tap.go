// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/containerd/plugin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	. "github.com/tencentcloud/CubeSandbox/Cubelet/network/types"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	localnetfile "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	ErrInvalidParams = errors.New("invalid network params")
)

const (
	eth0 = "eth0"
)

type Config struct {
	EthName      string `toml:"eth_name"`
	TapInitNum   int    `toml:"tap_init_num"`
	CIDR         string `toml:"cidr"`
	ObjectDir    string `toml:"object_dir"`
	MVMInnerIP   string `toml:"mvm_inner_ip"`
	MVMMacAddr   string `toml:"mvm_mac_addr"`
	MvmGwDestIP  string `toml:"mvm_gw_dest_ip"`
	MvmGwMacAddr string `toml:"mvm_gw_mac_addr"`
	MvmMask      int    `toml:"mvm_mask"`
	MvmMtu       int    `toml:"mvm_mtu"`

	CheckIntervalTime      tomlext.Duration `toml:"check_interval_in_sec"`
	ReportStatIntervalTime tomlext.Duration `toml:"report_stat_interval_in_sec"`
	AppMark                string           `toml:"app_mark"`

	WatchStream      bool             `toml:"watch_stream"`
	RedisConfPath    string           `toml:"redis_conf_path"`
	StreamNamePrefix string           `toml:"stream_name_prefix"`
	StreamKey        string           `toml:"stream_key"`
	StreamBlockTime  tomlext.Duration `toml:"stream_block_time"`

	RootPath string `toml:"root_path"`

	CubeEgressAdminURL    string           `toml:"cube_egress_admin_url"`
	CubeEgressPushTimeout tomlext.Duration `toml:"cube_egress_push_timeout"`
	CubeRouterEnable      bool             `toml:"cube_router_enable"`
	CubeRouterCIDR        string           `toml:"cube_router_cidr"`
	CubeRouterMacAddr     string           `toml:"cube_router_mac_addr"`
}

type local struct {
	Config          *Config
	allocationStore *networkstore.Store

	networkRuntime networkruntime.NetworkRuntime
}

func (l *local) SetAllocationStore(store *networkstore.Store) {
	l.allocationStore = store
}

// initTapPlugin wires the historical network plugin facade to the embedded
// network runtime. The plugin still owns workflow integration and shim-facing fd
// handoff, while TAP lifecycle, state, CubeVS and CubeEgress operations are now
// delegated to networkRuntime.
func initTapPlugin(ic *plugin.InitContext) (*local, error) {
	config, ok := ic.Config.(*Config)
	if !ok {
		return nil, ErrInvalidParams
	}
	ic.Context = context.WithValue(ic.Context, CubeLog.KeyCallee, "network")
	if config.CheckIntervalTime == 0 {
		config.CheckIntervalTime = tomlext.FromStdTime(5 * time.Second)
	}

	if config.ReportStatIntervalTime == 0 {
		config.ReportStatIntervalTime = tomlext.FromStdTime(60 * time.Second)
	}
	if config.MvmMask == 0 {
		config.MvmMask = 30
	}
	if net.ParseIP(config.MVMInnerIP) == nil {
		return nil, fmt.Errorf("invalid mvm_inner_ip: %q", config.MVMInnerIP)
	}
	if _, err := net.ParseMAC(config.MVMMacAddr); err != nil {
		return nil, err
	}
	if config.EthName == "" {
		config.EthName = eth0
	}

	log.G(ic.Context).Info("network plugin init begin")

	networkRuntime, err := networkruntime.NewNetworkController(networkRuntimeConfigFromPluginConfig(config))
	if err != nil {
		return nil, err
	}
	l := &local{
		Config:         config,
		networkRuntime: networkRuntime,
	}
	if err := l.waitForNetworkRuntimeReady(ic.Context); err != nil {
		return nil, fmt.Errorf("network runtime health check failed at init: %w", err)
	}
	log.G(ic.Context).Infof("network runtime initialized: service_type=%T", l.networkRuntime)
	return l, nil
}

func (l *local) Init(ctx context.Context, _ *workflow.InitInfo) error {
	log.G(ctx).Infof("Network Init")
	return nil
}

func networkRuntimeConfigFromPluginConfig(config *Config) networkruntime.Config {
	cfg := networkruntime.DefaultConfig()
	cfg.EthName = config.EthName
	cfg.ObjectDir = config.ObjectDir
	cfg.CIDR = config.CIDR
	cfg.MVMInnerIP = config.MVMInnerIP
	cfg.MVMMacAddr = config.MVMMacAddr
	cfg.MvmGwDestIP = config.MvmGwDestIP
	cfg.MvmGwMacAddr = config.MvmGwMacAddr
	cfg.MvmMask = config.MvmMask
	cfg.MvmMtu = config.MvmMtu
	cfg.TapInitNum = config.TapInitNum
	// Empty explicitly disables CubeEgress integration. Production config writes
	// the default loopback URL, so silently restoring a hidden default here would
	// make an intentional empty value impossible to express.
	cfg.CubeEgressAdminURL = config.CubeEgressAdminURL
	if config.CubeEgressPushTimeout != 0 {
		cfg.CubeEgressPushTimeout = tomlext.ToStdTime(config.CubeEgressPushTimeout)
	}
	cfg.CubeRouterEnable = config.CubeRouterEnable
	if config.CubeRouterCIDR != "" {
		cfg.CubeRouterCIDR = config.CubeRouterCIDR
	}
	if config.CubeRouterMacAddr != "" {
		cfg.CubeRouterMacAddr = config.CubeRouterMacAddr
	}
	return cfg
}

func (l *local) waitForNetworkRuntimeReady(ctx context.Context) error {
	return l.networkRuntime.Health(ctx)
}

// Create translates Cubelet workflow intent into a runtime EnsureNetwork call,
// then converts the runtime response back into the legacy ShimNetReq expected by
// the sandbox shim. This keeps the external workflow contract stable while moving
// host-side network ownership into networkRuntime.
func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) (err error) {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	request := opts.ReqInfo
	if request == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "RunCubeSandboxRequest nil")
	}
	req, err := decodeNetRequest(request.Annotations[constants.MasterAnnotationsNetWork])
	if err != nil {
		return err
	}
	log.G(ctx).Debugf("network request for %s: %s", opts.SandboxID, request.Annotations[constants.MasterAnnotationsNetWork])

	cubeNetworkConfigBeforeDNS, cubeNetworkConfig, resolvedDNSServers, dnsAllowOutCIDRs, err := buildRuntimePolicy(ctx, request)
	if err != nil {
		return err
	}
	log.G(ctx).Infof("tap create using network runtime: sandbox_id=%s request_id=%s exposed_ports=%v req_version=%d allow_internet_access=%s allow_out=%d deny_out=%d resolved_dns_servers=%v dns_allow_out_cidrs=%v cube_network_config_before_dns_merge=%s cube_network_config=%s",
		opts.SandboxID, request.GetRequestID(), request.ExposedPorts, req.Version,
		formatCubeNetworkAllowInternetAccess(cubeNetworkConfig), lenCubeNetworkList(cubeNetworkConfig, true), lenCubeNetworkList(cubeNetworkConfig, false),
		resolvedDNSServers, dnsAllowOutCIDRs, formatNetworkRuntimeCubeNetworkConfig(cubeNetworkConfigBeforeDNS), formatNetworkRuntimeCubeNetworkConfig(cubeNetworkConfig))

	// Record the sandbox's resolvers even when they were not merged into the
	// policy: this create had no domain target, but a later update may add one,
	// and by then the resolver list is not recoverable from anywhere else.
	ensureReq := l.buildEnsureNetworkRequestFromIntent(opts.SandboxID, request.GetRequestID(), request.ExposedPorts, req, cubeNetworkConfig,
		dnsServersToAllowOutCIDRs(ctx, resolvedDNSServers))
	log.G(ctx).Infof("tap create ensure request: sandbox_id=%s interfaces=%d routes=%d arps=%d port_mappings=%d resolved_dns_servers=%v dns_allow_out_cidrs=%v cube_network_config=%s persist_metadata=%s",
		ensureReq.SandboxID, len(ensureReq.Interfaces), len(ensureReq.Routes), len(ensureReq.ARPNeighbors),
		len(ensureReq.PortMappings), resolvedDNSServers, dnsAllowOutCIDRs, formatNetworkRuntimeCubeNetworkConfig(ensureReq.CubeNetworkConfig), utils.InterfaceToString(ensureReq.PersistMetadata))
	ensureResp, runtimeErr := l.networkRuntime.EnsureNetwork(ctx, ensureReq)
	if runtimeErr != nil {
		// Only an explicitly marked post-success error owns a committed lifecycle.
		// Rolling back every Ensure error would race a concurrent retry after the
		// runtime releases its sandbox lock and could delete the retry's network.
		if errors.Is(runtimeErr, networkruntime.ErrEnsureNetworkCommitted) {
			l.rollbackCreatedNetwork(ctx, opts.SandboxID, ensureReq, ensureResp)
		}
		return ret.Errorf(errorcode.ErrorCode_CreateNetworkFailed, "network runtime EnsureNetwork failed: %s", runtimeErr.Error())
	}

	defer func() {
		if err != nil {
			l.rollbackCreatedNetwork(ctx, opts.SandboxID, ensureReq, ensureResp)
		}
	}()

	log.G(ctx).Infof("tap create ensure response: sandbox_id=%s network_handle=%s interfaces=%d routes=%d arps=%d port_mappings=%d persist_metadata=%s",
		ensureResp.SandboxID, ensureResp.NetworkHandle, len(ensureResp.Interfaces), len(ensureResp.Routes),
		len(ensureResp.ARPNeighbors), len(ensureResp.PortMappings), utils.InterfaceToString(ensureResp.PersistMetadata))
	shimReq, err := l.buildShimNetReqFromEnsureResponse(ensureResp)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateNetworkFailed, "build shim req from network runtime response failed: %+v", err)
	}
	logShimNetwork(ctx, opts.SandboxID, shimReq)
	applyShimQoS(req, shimReq)
	opts.NetworkInfo = shimReq
	return nil
}

func decodeNetRequest(raw string) (*NetRequest, error) {
	req := &NetRequest{}
	if raw != "" {
		if err := utils.Decode(raw, req); err != nil {
			return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "decode network params failed: %+v, raw: %s", err, raw)
		}
	}
	return req, nil
}

func buildRuntimePolicy(ctx context.Context, request *cubebox.RunCubeSandboxRequest) (*networkruntime.CubeNetworkConfig, *networkruntime.CubeNetworkConfig, []string, []string, error) {
	beforeDNS := buildNetworkRuntimeCubeNetworkConfig(request)
	resolvedDNSServers, err := localnetfile.ResolveEffectiveDNSServers(request)
	if err != nil {
		return nil, nil, nil, nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "resolve effective dns servers failed: %v", err)
	}
	policy, dnsAllowOutCIDRs := mergeDNSAllowOutCIDRs(ctx, beforeDNS, resolvedDNSServers)
	return beforeDNS, policy, resolvedDNSServers, dnsAllowOutCIDRs, nil
}

func (l *local) rollbackCreatedNetwork(ctx context.Context, sandboxID string, ensureReq *networkruntime.EnsureNetworkRequest, ensureResp *networkruntime.EnsureNetworkResponse) {
	// EnsureNetwork may own TAP, CubeVS and CubeEgress state even when it reports
	// an error after the durable success point. Roll back with a detached bounded
	// context so parent cancellation does not leave host resources behind.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	networkHandle := sandboxID
	idempotencyKey := ""
	persistMetadata := map[string]string(nil)
	if ensureReq != nil {
		idempotencyKey = ensureReq.IdempotencyKey
		persistMetadata = ensureReq.PersistMetadata
	}
	if ensureResp != nil {
		if ensureResp.NetworkHandle != "" {
			networkHandle = ensureResp.NetworkHandle
		}
		persistMetadata = ensureResp.PersistMetadata
	}
	releaseReq := &networkruntime.ReleaseNetworkRequest{
		SandboxID:       sandboxID,
		NetworkHandle:   networkHandle,
		IdempotencyKey:  idempotencyKey,
		PersistMetadata: persistMetadata,
	}
	if _, err := l.networkRuntime.ReleaseNetwork(rollbackCtx, releaseReq); err != nil {
		log.G(rollbackCtx).Warnf("failed to release network during rollback for sandbox %s: %v", sandboxID, err)
	}
}

func logShimNetwork(ctx context.Context, sandboxID string, shimReq *ShimNetReq) {
	if len(shimReq.Interfaces) == 0 {
		log.G(ctx).Warnf("tap create shim net from network runtime returned no interfaces: sandbox_id=%s", sandboxID)
		return
	}
	intf := shimReq.Interfaces[0]
	log.G(ctx).Infof("tap create shim net from network runtime: sandbox_id=%s host_tap=%s sandbox_ip=%s guest_ip=%s mtu=%d port_mappings=%v",
		sandboxID, intf.Name, intf.IPAddr.String(), intf.IP, intf.Mtu, shimReq.PortMappings)
}

func applyShimQoS(req *NetRequest, shimReq *ShimNetReq) {
	if req == nil || req.Qos == nil || len(shimReq.Interfaces) == 0 {
		return
	}
	bandwidthQos := req.Qos.BandWidth
	opsQos := req.Qos.OPS
	shimReq.Interfaces[0].Qos = &QosConfig{
		BwSize:          bandwidthQos.Size,
		BwOneTimeBurst:  bandwidthQos.OneTimeBurst,
		BwRefillTime:    bandwidthQos.RefillTime,
		OpsSize:         opsQos.Size,
		OpsOneTimeBurst: opsQos.OneTimeBurst,
		OpsRefillTime:   opsQos.RefillTime,
	}
}

// Destroy releases the runtime-owned network state. The allocation store
// metadata is used as the compatibility bridge for older sandboxes whose shim
// request was persisted before this runtime refactor.
func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	sandboxID := opts.SandboxID

	var persistentMetadata []byte
	if l.allocationStore != nil {
		alloc, err := l.allocationStore.Get(opts.SandboxID)
		if err != nil {
			if !errors.Is(err, utils.ErrorKeyNotFound) {
				return err
			}
			log.G(ctx).Warnf("network allocation metadata %s not found; releasing runtime state anyway", opts.SandboxID)
		} else {
			persistentMetadata = alloc.PersistentMetadata
		}
	}
	requestID := ""
	if opts.DestroyInfo != nil {
		requestID = opts.DestroyInfo.GetRequestID()
	}
	if requestID == "" {
		requestID = uuid.New().String()
	}
	releaseReq := &networkruntime.ReleaseNetworkRequest{
		SandboxID:       sandboxID,
		NetworkHandle:   sandboxID,
		IdempotencyKey:  requestID,
		PersistMetadata: buildPersistMetadataMap(persistentMetadata, nil),
	}
	if _, err := l.networkRuntime.ReleaseNetwork(ctx, releaseReq); err != nil {
		return ret.Errorf(errorcode.ErrorCode_DestroyNetworkFailed, "network runtime ReleaseNetwork failed: %s", err.Error())
	}
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}
	requestID := ""
	trace := CubeLog.GetTraceInfo(ctx)
	if trace != nil {
		requestID = trace.RequestID
	}
	if requestID == "" {
		requestID = uuid.New().String()
		if trace != nil {
			trace = trace.DeepCopy()
			trace.RequestID = requestID
			ctx = CubeLog.WithRequestTrace(ctx, trace)
		}
	}

	sandboxID := opts.SandboxID
	log.G(ctx).Infof("network cleanup: sandbox_id=%s request_id=%s", sandboxID, requestID)
	if err := l.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: sandboxID,
		},
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			SandboxID: sandboxID,
			RequestID: requestID,
		},
	}); err != nil {
		log.G(ctx).Errorf("network cleanup failed: sandbox_id=%s err=%v", sandboxID, err)
		return err
	}
	return nil
}

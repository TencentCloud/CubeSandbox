// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/plugin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	. "github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	localnetfile "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrNotRangeIP = errors.New("NotRangeIP")

	ErrNotCubeTap = errors.New("NotCubeTap")

	ErrIPExhausted = errors.New("IP exhausted")

	ErrInvalidParams = errors.New("invalid network params")
)

const (
	tapNamePrefix = "z"
	cubeDev       = "cube-dev"
	eth0          = "eth0"
	retryNum      = 5
)

const (
	virtioNetHdrSize = 12
	txQLen           = 1000
)

func getGatewayMacAddr(itName string) (string, error) {
	link, err := netlink.LinkByName(itName)
	if err != nil {
		return "", err
	}
	neighs, err := netlink.NeighList(link.Attrs().Index, 0)
	if err != nil {
		return "", err
	}
	if len(neighs) < 1 {
		return "", fmt.Errorf("physics net card arp not unique. detail: %s", utils.InterfaceToString(neighs))
	}
	for _, neigh := range neighs {
		if neigh.Family == 2 && neigh.State == unix.NUD_REACHABLE {
			return neigh.HardwareAddr.String(), nil
		}
	}

	return "", errors.New("NotFound")
}

func addARPEntry(ip net.IP, mac string, cubeDevIndex int) error {
	macAddr, err := net.ParseMAC(mac)
	if err != nil {
		return err
	}

	return netlink.NeighSet(&netlink.Neigh{
		Family:       netlink.FAMILY_V4,
		IP:           ip,
		HardwareAddr: macAddr,
		LinkIndex:    cubeDevIndex,
		Flags:        0,
		State:        unix.NUD_PERMANENT,
		Type:         unix.RTN_UNSPEC,
	})
}

func getMachineDevice(itName string) (*MachineDevice, error) {
	link, err := netlink.LinkByName(itName)
	if err != nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	if len(addrs) != 1 {
		return nil, fmt.Errorf("NetAddrForIpv4 illegal:%s", addrs)
	}
	gwMac, err := getGatewayMacAddr(itName)
	if err != nil {
		return nil, err
	}
	gatewayMac, err := net.ParseMAC(gwMac)
	if err != nil {
		return nil, err
	}
	md := &MachineDevice{
		Index:      link.Attrs().Index,
		Name:       link.Attrs().Name,
		IP:         addrs[0].IP,
		Mac:        link.Attrs().HardwareAddr,
		GatewayMac: gatewayMac,
	}

	return md, nil
}

func getOrNewCubeDev(ip net.IP, mask, mtu int, macAddr string) (*CubeDev, error) {
	l, err := netlink.LinkByName(cubeDev)
	if err == nil {
		dummy, ok := l.(*netlink.Dummy)
		if !ok {
			return nil, errors.New(cubeDev + "is not Dummy")
		}
		dev := &CubeDev{
			Index: dummy.Index,
			Name:  cubeDev,
			IP:    ip,
			Mac:   dummy.HardwareAddr,
		}

		return dev, nil
	}
	gwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return nil, err
	}

	link := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         cubeDev,
			HardwareAddr: gwAddr,
			TxQLen:       txQLen,
		},
	}
	if err := netlink.LinkAdd(link); err != nil {
		return nil, err
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(mask, 32),
		},
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return nil, err
	}
	newLink, err := netlink.LinkByName(cubeDev)
	if err != nil {
		return nil, err
	}
	dummy := newLink.(*netlink.Dummy)
	dev := &CubeDev{
		Index: dummy.Index,
		Name:  cubeDev,
		IP:    ip,
		Mac:   dummy.HardwareAddr,
	}

	return dev, nil
}

func isTapUsing(ifIndex int) (bool, error) {
	link, err := netlink.LinkByIndex(ifIndex)
	if err != nil {
		return false, err
	}
	isUsing := link.Attrs().RawFlags&unix.IFF_LOWER_UP > 0

	return isUsing, nil
}

func listCubeTaps() (map[string]*Tap, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	ip2tap := make(map[string]*Tap, 0)
	for _, link := range links {
		tap, ok := link.(*netlink.Tuntap)
		if !ok {
			continue
		}
		if tap.Mode != netlink.TUNTAP_MODE_TAP {
			continue
		}
		ipStr, err := extractIP(tap.Name)
		if err != nil {
			continue
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		ip = ip.To4()
		isUsing := link.Attrs().RawFlags&unix.IFF_LOWER_UP > 0
		ip2tap[ip.String()] = &Tap{
			Name:    tap.Name,
			Index:   tap.Index,
			IP:      ip,
			IsUsing: isUsing,
			File:    nil,
		}
	}

	return ip2tap, nil
}

func geneTapName(ip string) string {
	return tapNamePrefix + ip
}

func extractIP(name string) (string, error) {
	idx := strings.Index(name, tapNamePrefix)
	if idx != 0 {
		return "", ErrNotCubeTap
	}

	return name[len(tapNamePrefix):], nil
}

var (
	Name2MvmNet sync.Map
	CfgAppMark  string

	tapVersion uint32 = 0
)

type Config struct {
	EthName             string   `toml:"eth_name"`
	TapInitNum          int      `toml:"tap_init_num"`
	CIDR                string   `toml:"cidr"`
	ObjectDir           string   `toml:"object_dir"`
	MVMInnerIP          string   `toml:"mvm_inner_ip"`
	MVMMacAddr          string   `toml:"mvm_mac_addr"`
	MvmGwDestIP         string   `toml:"mvm_gw_dest_ip"`
	MvmGwMacAddr        string   `toml:"mvm_gw_mac_addr"`
	MvmMask             int      `toml:"mvm_mask"`
	MvmMtu              int      `toml:"mvm_mtu"`
	DefaultExposedPorts []uint16 `toml:"default_exposed_ports"`

	CheckIntervalTime      tomlext.Duration `toml:"check_interval_in_sec"`
	ReportStatIntervalTime tomlext.Duration `toml:"report_stat_interval_in_sec"`
	AppMark                string           `toml:"app_mark"`

	WatchStream      bool             `toml:"watch_stream"`
	RedisConfPath    string           `toml:"redis_conf_path"`
	StreamNamePrefix string           `toml:"stream_name_prefix"`
	StreamKey        string           `toml:"stream_key"`
	StreamBlockTime  tomlext.Duration `toml:"stream_block_time"`

	RootPath string `toml:"root_path"`

	EnableNetworkAgent        bool             `toml:"enable_network_agent"`
	NetworkAgentEndpoint      string           `toml:"network_agent_endpoint"`
	NetworkAgentTapSocket     string           `toml:"network_agent_tap_socket"`
	NetworkAgentInitTimeout   tomlext.Duration `toml:"network_agent_init_timeout"`
	NetworkAgentRetryInterval tomlext.Duration `toml:"network_agent_retry_interval"`

	ReconcileInterval tomlext.Duration `toml:"reconcile_interval"`
}

type local struct {
	ID2MvmNet       sync.Map
	allocator       *IPAllocator
	portAllocator   allocator.Allocator[uint16]
	Config          *Config
	cubeDev         *CubeDev
	Device          *MachineDevice
	DestroyLocks    *utils.ResourceLocks
	allocationStore *networkstore.Store

	networkAgentClient networkagentclient.Client

	networkAgentConnectFailTotal  uint64
	networkAgentTimeoutFailTotal  uint64
	networkAgentBusinessFailTotal uint64
}

func (l *local) SetAllocationStore(store *networkstore.Store) {
	l.allocationStore = store
}

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
	if config.NetworkAgentEndpoint == "" {
		config.NetworkAgentEndpoint = "grpc+unix:///tmp/cube/network-agent-grpc.sock"
	}
	if config.NetworkAgentTapSocket == "" {
		config.NetworkAgentTapSocket = "/tmp/cube/network-agent-tap.sock"
	}
	if config.NetworkAgentInitTimeout == 0 {
		config.NetworkAgentInitTimeout = tomlext.FromStdTime(30 * time.Second)
	}
	if config.NetworkAgentRetryInterval == 0 {
		config.NetworkAgentRetryInterval = tomlext.FromStdTime(time.Second)
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

	if len(config.DefaultExposedPorts) > 0 {
		log.G(ic.Context).Errorf("set default exposed ports to %v", config.DefaultExposedPorts)
		DefaultExposedPorts = config.DefaultExposedPorts
	}
	log.G(ic.Context).Info("network plugin init begin")

	device, err := getMachineDevice(config.EthName)
	if err != nil {
		return nil, err
	}
	log.G(ic.Context).Info("network get node info done")
	ipAllocator, err := NewAllocator(config.CIDR)
	if err != nil {
		return nil, err
	}
	log.G(ic.Context).Info("network ipam init done")

	portAllocator, err := initPortAllocatorFromSysConfig()
	if err != nil {
		return nil, err
	}
	log.G(ic.Context).Info("network port allocator init done")

	gwIP := ipAllocator.GatewayIP()
	cubeDev, err := getOrNewCubeDev(gwIP, ipAllocator.mask, config.MvmMtu, config.MvmGwMacAddr)
	if err != nil {
		return nil, err
	}
	log.G(ic.Context).Info("network cube-dev init done")

	l := &local{
		Config:             config,
		Device:             device,
		cubeDev:            cubeDev,
		allocator:          ipAllocator,
		portAllocator:      portAllocator,
		DestroyLocks:       utils.NewResourceLocks(),
		networkAgentClient: networkagentclient.NewNoopClient(),
	}
	naClient, naErr := networkagentclient.NewClient(config.NetworkAgentEndpoint)
	if naErr != nil {
		log.G(ic.Context).Warnf("create network-agent client failed, fallback to noop: %v", naErr)
	} else {
		l.networkAgentClient = naClient
	}
	log.G(ic.Context).Infof("tap plugin init: enable_network_agent=%t endpoint=%s tap_socket=%s client_type=%T",
		config.EnableNetworkAgent, config.NetworkAgentEndpoint, config.NetworkAgentTapSocket, l.networkAgentClient)
	initErr := l.waitForNetworkAgentReady(ic.Context)
	if initErr != nil {
		if !errors.Is(initErr, networkagentclient.ErrNotConfigured) {
			l.recordNetworkAgentFailure(initErr)
		}
		return nil, fmt.Errorf("network-agent health check failed at init, endpoint=%s: %w", config.NetworkAgentEndpoint, initErr)
	}
	log.G(ic.Context).Infof("network-agent health check passed at init, endpoint=%s", config.NetworkAgentEndpoint)
	return l, nil
}

func (l *local) Init(ctx context.Context, _ *workflow.InitInfo) error {
	log.G(ctx).Infof("Network Init")
	return nil
}

func (l *local) waitForNetworkAgentReady(ctx context.Context) error {
	timeout := time.Duration(l.Config.NetworkAgentInitTimeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	retryInterval := time.Duration(l.Config.NetworkAgentRetryInterval)
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		healthCtx, healthCancel := context.WithTimeout(deadlineCtx, time.Second)
		err := l.networkAgentClient.Health(healthCtx, &networkagentclient.HealthRequest{})
		healthCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		log.G(ctx).Warnf("network-agent not ready yet, endpoint=%s retry_interval=%s err=%v",
			l.Config.NetworkAgentEndpoint, retryInterval, err)

		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("waited %s for network-agent readiness: %w", timeout, lastErr)
		case <-time.After(retryInterval):
		}
	}
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) (err error) {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	request := opts.ReqInfo

	netRequest := request.Annotations[constants.MasterAnnotationsNetWork]
	log.G(ctx).Debugf("network request for %s: %s", opts.SandboxID, netRequest)

	req := &NetRequest{}
	if netRequest != "" {
		err := utils.Decode(netRequest, req)
		if err != nil {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "decode network params failed: %+v, raw: %s",
				err, netRequest)
		}
	}
	if err := req.Validate(); err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "validate network params failed: %v", err)
	}

	cubeVSContextBeforeDNS := buildNetworkAgentCubeVSContext(request)
	resolvedDNSServers, err := localnetfile.ResolveEffectiveDNSServers(request)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "resolve effective dns servers failed: %v", err)
	}
	cubeVSContext, dnsAllowOutCIDRs := mergeDNSAllowOutCIDRs(cubeVSContextBeforeDNS, resolvedDNSServers)

	log.G(ctx).Infof("tap create using network-agent: sandbox_id=%s request_id=%s exposed_ports=%v req_version=%d allow_internet_access=%s allow_out=%d deny_out=%d resolved_dns_servers=%v dns_allow_out_cidrs=%v cubevs_context_before_dns_merge=%s cubevs_context=%s",
		opts.SandboxID, request.GetRequestID(), request.ExposedPorts, req.Version,
		formatCubeVSAllowInternetAccess(cubeVSContext), lenCubeVSList(cubeVSContext, true), lenCubeVSList(cubeVSContext, false),
		resolvedDNSServers, dnsAllowOutCIDRs, formatNetworkAgentCubeVSContext(cubeVSContextBeforeDNS), formatNetworkAgentCubeVSContext(cubeVSContext))
	ensureReq := l.buildEnsureNetworkRequestFromIntent(opts.SandboxID, request.GetRequestID(), request.ExposedPorts, req, cubeVSContext)
	log.G(ctx).Infof("tap create ensure request: sandbox_id=%s interfaces=%d routes=%d arps=%d port_mappings=%d resolved_dns_servers=%v dns_allow_out_cidrs=%v cubevs_context=%s persist_metadata=%s",
		ensureReq.SandboxID, len(ensureReq.Interfaces), len(ensureReq.Routes), len(ensureReq.ARPNeighbors),
		len(ensureReq.PortMappings), resolvedDNSServers, dnsAllowOutCIDRs, formatNetworkAgentCubeVSContext(ensureReq.CubeVSContext), utils.InterfaceToString(ensureReq.PersistMetadata))
	ensureResp, naErr := l.networkAgentClient.EnsureNetwork(ctx, ensureReq)
	if naErr != nil {
		l.recordNetworkAgentFailure(naErr)
		return ret.Errorf(errorcode.ErrorCode_CreateNetworkFailed, "network-agent EnsureNetwork failed: %s", classifyNetworkAgentError(naErr))
	}
	log.G(ctx).Infof("tap create ensure response: sandbox_id=%s network_handle=%s interfaces=%d routes=%d arps=%d port_mappings=%d persist_metadata=%s",
		ensureResp.SandboxID, ensureResp.NetworkHandle, len(ensureResp.Interfaces), len(ensureResp.Routes),
		len(ensureResp.ARPNeighbors), len(ensureResp.PortMappings), utils.InterfaceToString(ensureResp.PersistMetadata))
	shimReq, err := l.buildShimNetReqFromEnsureResponse(ensureResp, req)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateNetworkFailed, "build shim req from network-agent response failed: %+v", err)
	}
	if err := l.registerNetworkAgentTapForPool(ctx, opts.SandboxID, shimReq); err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateNetworkFailed, "register network-agent tap for pool failed: %+v", err)
	}
	if len(shimReq.Interfaces) > 0 {
		intf := shimReq.Interfaces[0]
		log.G(ctx).Infof("tap create shim net from network-agent: sandbox_id=%s host_tap=%s sandbox_ip=%s guest_ip=%s mtu=%d queues=%d port_mappings=%v",
			opts.SandboxID, intf.Name, intf.IPAddr.String(), intf.IP, intf.Mtu, shimReq.Queues, shimReq.PortMappings)
	} else {
		log.G(ctx).Warnf("tap create shim net from network-agent returned no interfaces: sandbox_id=%s", opts.SandboxID)
	}
	if req.Qos != nil && len(shimReq.Interfaces) > 0 {
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
	opts.NetworkInfo = shimReq
	return nil
}

func buildNetworkAgentCubeVSContext(request *cubebox.RunCubeSandboxRequest) *networkagentclient.CubeVSContext {
	if request == nil {
		return nil
	}
	if request.GetCubevsContext() != nil {
		return mapRunRequestCubeVSContext(request.GetCubevsContext())
	}
	return buildLegacyNetworkAgentCubeVSContext(request.GetAnnotations())
}

func mapRunRequestCubeVSContext(in *cubebox.CubeVSContext) *networkagentclient.CubeVSContext {
	if in == nil {
		return nil
	}
	out := &networkagentclient.CubeVSContext{
		AllowOut: append([]string(nil), in.GetAllowOut()...),
		DenyOut:  append([]string(nil), in.GetDenyOut()...),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := in.GetAllowInternetAccess()
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func buildLegacyNetworkAgentCubeVSContext(annotations map[string]string) *networkagentclient.CubeVSContext {
	if len(annotations) == 0 {
		return nil
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyBlockAll]; ok && v == "true" {
		allowInternetAccess := false
		return &networkagentclient.CubeVSContext{AllowInternetAccess: &allowInternetAccess}
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyAllowPublicServices]; ok && v == "true" {
		allowInternetAccess := true
		return &networkagentclient.CubeVSContext{AllowInternetAccess: &allowInternetAccess}
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyDefault]; ok && v == "true" {
		allowInternetAccess := true
		return &networkagentclient.CubeVSContext{AllowInternetAccess: &allowInternetAccess}
	}
	return nil
}

func formatCubeVSAllowInternetAccess(ctx *networkagentclient.CubeVSContext) string {
	if ctx == nil || ctx.AllowInternetAccess == nil {
		return "default(true)"
	}
	if *ctx.AllowInternetAccess {
		return "true"
	}
	return "false"
}

func lenCubeVSList(ctx *networkagentclient.CubeVSContext, allow bool) int {
	if ctx == nil {
		return 0
	}
	if allow {
		return len(ctx.AllowOut)
	}
	return len(ctx.DenyOut)
}

func formatNetworkAgentCubeVSContext(ctx *networkagentclient.CubeVSContext) string {
	if ctx == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[]"
	}
	return fmt.Sprintf(
		"allow_internet_access=%s allow_out=%v deny_out=%v",
		formatCubeVSAllowInternetAccess(ctx),
		ctx.AllowOut,
		ctx.DenyOut,
	)
}

func mergeDNSAllowOutCIDRs(ctx *networkagentclient.CubeVSContext, dnsServers []string) (*networkagentclient.CubeVSContext, []string) {
	if !shouldAppendDNSAllowOut(ctx) || len(dnsServers) == 0 {
		return ctx, nil
	}
	out := cloneNetworkAgentCubeVSContext(ctx)
	if out == nil {
		out = &networkagentclient.CubeVSContext{}
	}
	dnsAllowOutCIDRs := make([]string, 0, len(dnsServers))
	for _, dnsServer := range dnsServers {
		cidr, ok := dnsServerToCIDR(dnsServer)
		if !ok {
			continue
		}
		dnsAllowOutCIDRs = append(dnsAllowOutCIDRs, cidr)
	}
	out.AllowOut = appendUniqueString(out.AllowOut, dnsAllowOutCIDRs)
	return out, dnsAllowOutCIDRs
}

func shouldAppendDNSAllowOut(ctx *networkagentclient.CubeVSContext) bool {
	if ctx == nil {
		return false
	}
	if ctx.AllowInternetAccess != nil && !*ctx.AllowInternetAccess {
		return true
	}
	return len(ctx.AllowOut) > 0 || len(ctx.DenyOut) > 0
}

func cloneNetworkAgentCubeVSContext(ctx *networkagentclient.CubeVSContext) *networkagentclient.CubeVSContext {
	if ctx == nil {
		return nil
	}
	out := &networkagentclient.CubeVSContext{
		AllowOut: append([]string(nil), ctx.AllowOut...),
		DenyOut:  append([]string(nil), ctx.DenyOut...),
	}
	if ctx.AllowInternetAccess != nil {
		v := *ctx.AllowInternetAccess
		out.AllowInternetAccess = &v
	}
	return out
}

func dnsServerToCIDR(ip string) (string, bool) {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return "", false
	}
	if ipv4 := parsedIP.To4(); ipv4 != nil {
		return ipv4.String() + "/32", true
	}
	return parsedIP.String() + "/128", true
}

func appendUniqueString(base []string, extra []string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := append([]string(nil), base...)
	for _, item := range base {
		seen[item] = struct{}{}
	}
	for _, item := range extra {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	sandboxID := opts.SandboxID

	unlock := l.DestroyLocks.Lock(sandboxID)
	defer unlock()

	mvmNet := l.loadNet(sandboxID)
	if mvmNet == nil {
		return nil
	}

	alloc, err := l.allocationStore.Get(opts.SandboxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) {
			log.G(ctx).Errorf("network %s not found", opts.SandboxID)
			return nil
		}
		return err
	}
	shimNetReq := &ShimNetReq{}
	shimNetReq.FromPersistMetadata(alloc.PersistentMetadata)
	requestID := ""
	if opts.DestroyInfo != nil {
		requestID = opts.DestroyInfo.GetRequestID()
	}
	if requestID == "" {
		requestID = uuid.New().String()
	}
	releaseReq := &networkagentclient.ReleaseNetworkRequest{
		SandboxID:       sandboxID,
		NetworkHandle:   sandboxID,
		IdempotencyKey:  requestID,
		PersistMetadata: buildPersistMetadataMap(alloc.PersistentMetadata, shimNetReq),
	}
	if err := l.networkAgentClient.ReleaseNetwork(ctx, releaseReq); err != nil {
		l.recordNetworkAgentFailure(err)
		return ret.Errorf(errorcode.ErrorCode_DestroyNetworkFailed, "network-agent ReleaseNetwork failed: %s", classifyNetworkAgentError(err))
	}
	l.unregisterNetworkAgentTapForPool(ctx, sandboxID)
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}
	requestID := ""
	rt := CubeLog.GetTraceInfo(ctx)
	if rt != nil {
		requestID = rt.RequestID
	}
	if requestID == "" {
		requestID = uuid.New().String()
		rt = rt.DeepCopy()
		rt.RequestID = requestID
		ctx = CubeLog.WithRequestTrace(ctx, rt)
	}

	log.G(ctx).Errorf("plugin_tap CleanUp")
	sandBoxID := opts.SandboxID

	if err := l.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: sandBoxID,
		},
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			SandboxID: sandBoxID,
			RequestID: requestID,
		},
	}); err != nil {

		log.G(ctx).Errorf("CleanUp fail:%v", err)
		return err
	}
	return nil
}

func (l *local) buildEnsureNetworkRequestFromIntent(sandboxID, requestID string, exposedPorts []int64, shimReq *NetRequest, cubeVSContext *networkagentclient.CubeVSContext) *networkagentclient.EnsureNetworkRequest {
	desired := &networkagentclient.EnsureNetworkRequest{
		SandboxID:      sandboxID,
		IdempotencyKey: requestID,
		Interfaces: []networkagentclient.Interface{
			{
				MAC:     l.Config.MVMMacAddr,
				MTU:     int32(l.Config.MvmMtu),
				IPs:     []string{fmt.Sprintf("%s/%d", l.Config.MVMInnerIP, l.Config.MvmMask)},
				Gateway: l.Config.MvmGwDestIP,
			},
		},
		Routes: []networkagentclient.Route{
			{
				Gateway: l.Config.MvmGwDestIP,
				Device:  eth0,
			},
		},
		ARPNeighbors: []networkagentclient.ARPNeighbor{
			{
				IP:     l.Config.MvmGwDestIP,
				MAC:    l.Config.MvmGwMacAddr,
				Device: eth0,
			},
		},
	}
	desired.CubeVSContext = cubeVSContext
	portReq := make(map[uint16]struct{})
	for _, port := range exposedPorts {
		portReq[uint16(port)] = struct{}{}
	}
	for _, port := range DefaultExposedPorts {
		portReq[port] = struct{}{}
	}
	for reqPort := range portReq {
		desired.PortMappings = append(desired.PortMappings, networkagentclient.PortMapping{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			ContainerPort: int32(reqPort),
		})
	}
	desired.PersistMetadata = buildPersistMetadataMap(nil, nil)
	desired.PersistMetadata["gateway_ip"] = l.Config.MvmGwDestIP
	desired.PersistMetadata["mvm_inner_ip"] = l.Config.MVMInnerIP
	if shimReq != nil && shimReq.Qos != nil {
		desired.PersistMetadata["qos_enabled"] = "true"
	}
	return desired
}

func (l *local) buildShimNetReqFromEnsureResponse(resp *networkagentclient.EnsureNetworkResponse, _ *NetRequest) (*ShimNetReq, error) {
	if resp == nil {
		return nil, fmt.Errorf("network-agent response is nil")
	}
	if len(resp.Interfaces) == 0 {
		return nil, fmt.Errorf("network-agent returned no interfaces")
	}
	sandboxIP := net.ParseIP(resp.PersistMetadata["sandbox_ip"]).To4()
	if sandboxIP == nil {
		return nil, fmt.Errorf("network-agent response missing sandbox_ip")
	}
	intf := resp.Interfaces[0]
	mvmIPs := make([]MVMIp, 0, len(intf.IPs))
	legacyIP := l.Config.MVMInnerIP
	legacyMask := l.Config.MvmMask
	for _, cidr := range intf.IPs {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("parse interface cidr %q: %w", cidr, err)
		}
		mask, _ := network.Mask.Size()
		mvmIPs = append(mvmIPs, MVMIp{
			IP:     ip.String(),
			Mask:   mask,
			Family: 0,
		})
		if len(mvmIPs) == 1 {
			legacyIP = ip.String()
			legacyMask = mask
		}
	}
	shimReq := &ShimNetReq{
		Interfaces: []*Interface{
			{
				Name:      intf.Name,
				IPAddr:    sandboxIP,
				GuestName: eth0,
				Mac:       intf.MAC,
				Mtu:       int(intf.MTU),
				IP:        legacyIP,
				Family:    0,
				Mask:      legacyMask,
				IPs:       mvmIPs,
			},
		},
	}
	for _, route := range resp.Routes {
		shimReq.Routes = append(shimReq.Routes, Route{
			Family:  0,
			Dest:    route.Destination,
			Gateway: route.Gateway,
			Source:  legacyIP,
			Device:  route.Device,
			Scope:   0,
		})
	}
	for _, arp := range resp.ARPNeighbors {
		shimReq.ARPs = append(shimReq.ARPs, ARP{
			DestIP: arp.IP,
			Device: arp.Device,
			LlAddr: arp.MAC,
		})
	}
	for _, pm := range resp.PortMappings {
		shimReq.PortMappings = append(shimReq.PortMappings, PortMapping{
			HostPort:      uint16(pm.HostPort),
			ContainerPort: uint16(pm.ContainerPort),
		})
	}
	if len(shimReq.Routes) == 0 {
		shimReq.Routes = []Route{{
			Family:  0,
			Gateway: l.Config.MvmGwDestIP,
			Source:  legacyIP,
			Device:  eth0,
			Scope:   0,
		}}
	}
	if len(shimReq.ARPs) == 0 {
		shimReq.ARPs = []ARP{{
			DestIP: l.Config.MvmGwDestIP,
			Device: eth0,
			LlAddr: l.Config.MvmGwMacAddr,
		}}
	}
	return shimReq, nil
}

func buildPersistMetadataMap(raw []byte, shimReq *ShimNetReq) map[string]string {
	meta := map[string]string{
		"shim_req_metadata_b64": base64.StdEncoding.EncodeToString(raw),
	}
	if shimReq != nil {
		if ip := shimReq.SandboxIP(); ip != "" {
			meta["sandbox_ip"] = ip
		}
		if gw := shimReq.GatewayIP(); gw != "" {
			meta["gateway_ip"] = gw
		}
	}
	return meta
}

func classifyNetworkAgentError(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		return fmt.Sprintf("grpc_code=%s grpc_msg=%s", st.Code().String(), st.Message())
	}
	return err.Error()
}

func classifyNetworkAgentFailureClass(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable:
			return "connect"
		case codes.DeadlineExceeded:
			return "timeout"
		default:
			return "business"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "deadline exceeded") || strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "context deadline exceeded") {
		return "timeout"
	}
	if strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "no such file or directory") || strings.Contains(errStr, "transport is closing") {
		return "connect"
	}
	return "business"
}

func (l *local) recordNetworkAgentFailure(err error) {
	switch classifyNetworkAgentFailureClass(err) {
	case "connect":
		atomic.AddUint64(&l.networkAgentConnectFailTotal, 1)
	case "timeout":
		atomic.AddUint64(&l.networkAgentTimeoutFailTotal, 1)
	case "business":
		atomic.AddUint64(&l.networkAgentBusinessFailTotal, 1)
	}
}

func (l *local) storeNet(mvmNet *MvmNet) {
	l.ID2MvmNet.Store(mvmNet.ID, mvmNet)
	Name2MvmNet.Store(mvmNet.Name, mvmNet)
}

func (l *local) delNet(mvmNet *MvmNet) {
	if mvmNet == nil {
		return
	}
	l.ID2MvmNet.Delete(mvmNet.ID)
	if mvmNet.Tap != nil {
		Name2MvmNet.Delete(mvmNet.Name)
	}
}

func (l *local) registerNetworkAgentTapForPool(ctx context.Context, sandboxID string, shimReq *ShimNetReq) error {
	if shimReq == nil || len(shimReq.Interfaces) == 0 {
		return fmt.Errorf("shim network interfaces are empty")
	}
	intf := shimReq.Interfaces[0]
	if intf.Name == "" {
		return fmt.Errorf("shim network tap name is empty")
	}
	if intf.IPAddr == nil {
		return fmt.Errorf("shim network sandbox ip is empty")
	}
	file, err := requestNetworkAgentTapFile(l.Config.NetworkAgentTapSocket, sandboxID, intf.Name)
	if err != nil {
		return fmt.Errorf("request original tap fd for %s: %w", intf.Name, err)
	}
	link, err := netlink.LinkByName(intf.Name)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("lookup tap link %s: %w", intf.Name, err)
	}
	tap := &Tap{
		Index: link.Attrs().Index,
		Name:  intf.Name,
		IP:    append(net.IP(nil), intf.IPAddr...),
		File:  file,
	}
	tap.SetPortMappings(shimReq.PortMappings)

	if old := l.loadNet(sandboxID); old != nil {
		l.delNet(old)
		if old.Tap != nil && old.Tap.File != nil && old.Tap.File != file {
			_ = old.Tap.File.Close()
		}
	}

	mvmNet := &MvmNet{
		ID:  sandboxID,
		Tap: tap,
	}
	l.storeNet(mvmNet)
	log.G(ctx).Infof("registered network-agent tap for fd pool: sandbox_id=%s tap_name=%s ifindex=%d fd=%d sandbox_ip=%s port_mappings=%v",
		sandboxID, tap.Name, tap.Index, tap.File.Fd(), tap.IP.String(), shimReq.PortMappings)
	return nil
}

func (l *local) unregisterNetworkAgentTapForPool(ctx context.Context, sandboxID string) {
	mvmNet := l.loadNet(sandboxID)
	if mvmNet == nil {
		return
	}
	l.delNet(mvmNet)
	if mvmNet.Tap != nil && mvmNet.Tap.File != nil {
		log.G(ctx).Infof("unregister network-agent tap from fd pool: sandbox_id=%s tap_name=%s fd=%d",
			sandboxID, mvmNet.Tap.Name, mvmNet.Tap.File.Fd())
		_ = mvmNet.Tap.File.Close()
		mvmNet.Tap.File = nil
	}
}

type networkAgentTapFDRequest struct {
	Name      string `json:"name"`
	SandboxID string `json:"sandboxId"`
}

type networkAgentTapFDResponse struct {
	ErrCode string `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
}

func requestNetworkAgentTapFile(socketPath, sandboxID, tapName string) (*os.File, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("network-agent tap socket is empty")
	}
	addr := &net.UnixAddr{Name: socketPath, Net: "unix"}
	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(&networkAgentTapFDRequest{
		Name:      tapName,
		SandboxID: sandboxID,
	})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(reqBody); err != nil {
		return nil, err
	}
	buf := make([]byte, 1024)
	oob := make([]byte, 1024)
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	resp := &networkAgentTapFDResponse{}
	if err := json.Unmarshal(buf[:n], resp); err != nil {
		return nil, err
	}
	if resp.ErrCode != "" && resp.ErrCode != "0" {
		return nil, errors.New(resp.ErrMsg)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		fds, err := syscall.ParseUnixRights(&msg)
		if err != nil {
			return nil, err
		}
		if len(fds) == 0 {
			continue
		}
		return os.NewFile(uintptr(fds[0]), "/dev/net/tun"), nil
	}
	return nil, fmt.Errorf("network-agent tap fd response missing fd")
}

func (l *local) loadNet(sandboxID string) *MvmNet {
	mvmNet, exist := l.ID2MvmNet.Load(sandboxID)
	if !exist {
		return nil
	}

	return mvmNet.(*MvmNet)
}

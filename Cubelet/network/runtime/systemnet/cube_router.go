// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var execCommand = exec.Command

const (
	cubeRouterName     = "cube-router"
	sandboxCIDRMinMask = 16
	sandboxCIDRMaxMask = 24
	tcpPortMax         = 65535
)

// CubeRouter is the live host dummy device used by route-aware egress.
type CubeRouter struct {
	Index        int
	Name         string
	IP           net.IP
	Mask         int
	Mac          net.HardwareAddr
	NATIP        net.IP
	RoutedPrefix bool
}

// CubeRouterConfig is the systemnet boundary for deriving the cube-router
// desired state without importing the runtime package's Config type.
type CubeRouterConfig struct {
	SandboxCIDR string
	RouterCIDR  string
	MacAddr     string
}

// CubeRouterSpec is the desired cube-router configuration derived from config.
// RoutedPrefix distinguishes an explicit router CIDR from the fallback mode that
// uses two reserved addresses at the end of the sandbox CIDR.
type CubeRouterSpec struct {
	IP           net.IP
	Mask         int
	Mac          string
	NATIP        net.IP
	RoutedPrefix bool
}

// CubeRouterSpecFromConfig chooses the explicit router CIDR when configured, or
// the sandbox-CIDR fallback when it is omitted.
func CubeRouterSpecFromConfig(cfg CubeRouterConfig) (*CubeRouterSpec, error) {
	if cfg.RouterCIDR != "" {
		return deriveCubeRouterCIDRSpec(cfg.RouterCIDR, cfg.MacAddr)
	}
	return deriveCubeRouterSpecFromSandboxCIDR(cfg.SandboxCIDR, cfg.MacAddr)
}

// GetOrCreateCubeRouter creates or reconciles the cube-router dummy link. It is
// intentionally strict about existing device type/MAC/address so a stale or
// manually-created host device cannot silently capture sandbox egress traffic.
func GetOrCreateCubeRouter(spec *CubeRouterSpec, mtu int) (*CubeRouter, error) {
	if err := validateCubeRouterSpec(spec); err != nil {
		return nil, err
	}
	desiredAddr := cubeRouterAddr(spec)
	link, err := netlinkLinkByName(cubeRouterName)
	if err == nil {
		dummy, ok := link.(*netlink.Dummy)
		if !ok {
			return nil, fmt.Errorf("%s is not dummy", cubeRouterName)
		}
		return reconcileCubeRouter(dummy, spec, desiredAddr, mtu)
	}
	if !isLinkNotFound(err) {
		return nil, fmt.Errorf("lookup %s: %w", cubeRouterName, err)
	}
	return createCubeRouter(spec, desiredAddr, mtu)
}

// EnsureCubeRouterMatches removes stale cube-router state when config changed
// between restarts, letting GetOrCreateCubeRouter recreate the desired device.
func EnsureCubeRouterMatches(spec *CubeRouterSpec, snatPortMin uint16) error {
	existing, err := currentCubeRouter()
	if err != nil || existing == nil {
		return err
	}
	wantMac, err := net.ParseMAC(spec.Mac)
	if err != nil {
		return err
	}
	if existing.IP.Equal(spec.IP) &&
		existing.Mask == spec.Mask &&
		existing.NATIP.Equal(spec.NATIP) &&
		existing.RoutedPrefix == spec.RoutedPrefix &&
		existing.Mac.String() == wantMac.String() {
		return nil
	}
	return CleanupCubeRouter(snatPortMin)
}

// CleanupCubeRouter deletes cube-router and its host networking rules when
// route-aware egress is disabled or the desired spec changed.
func CleanupCubeRouter(snatPortMin uint16) error {
	link, err := netlinkLinkByName(cubeRouterName)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return err
	}
	router, err := currentCubeRouter()
	if err != nil {
		return err
	}
	if router != nil && router.IP != nil && router.NATIP != nil {
		if err := deleteCubeRouterHostNetworking(router, snatPortMin); err != nil {
			return err
		}
	}
	return netlinkLinkDel(link)
}

// ConfigureCubeRouterHostNetworking enables forwarding and installs the route,
// iptables, and neighbor state required for CubeVS route-aware egress.
func ConfigureCubeRouterHostNetworking(router *CubeRouter, snatPortMin uint16) error {
	if router == nil {
		return fmt.Errorf("cube-router is not initialized")
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward failed: %w", err)
	}
	if !router.RoutedPrefix {
		if err := ensureRouteToCubeRouterNAT(router); err != nil {
			return err
		}
	}
	if err := ensureCubeRouterIptables(router, snatPortMin); err != nil {
		return err
	}
	if err := ensureCubeRouterNATNeighbor(router); err != nil {
		return err
	}
	return nil
}

// deriveCubeRouterCIDRSpec derives router local IP and NAT IP from an explicit
// cube-router CIDR. The first usable address is the dummy device, the second is
// the NAT identity used by CubeVS egress.
func deriveCubeRouterCIDRSpec(cidr, macAddr string) (*CubeRouterSpec, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse cube-router cidr %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil || network.IP.To4() == nil {
		return nil, fmt.Errorf("cube-router cidr %q is not IPv4", cidr)
	}
	if !ip4.Equal(network.IP.To4()) {
		return nil, fmt.Errorf("cube-router cidr %q must be aligned to the network address", cidr)
	}
	mask, bits := network.Mask.Size()
	if bits != 32 || mask < sandboxCIDRMinMask || mask > 30 {
		return nil, fmt.Errorf("cube-router cidr %q mask must be between /%d and /30", cidr, sandboxCIDRMinMask)
	}
	if _, err := net.ParseMAC(macAddr); err != nil {
		return nil, fmt.Errorf("parse cube-router mac %q: %w", macAddr, err)
	}

	base := ipv4ToUint32(network.IP)
	return &CubeRouterSpec{
		IP:           uint32ToIPv4(base + 1),
		Mask:         mask,
		Mac:          macAddr,
		NATIP:        uint32ToIPv4(base + 2),
		RoutedPrefix: true,
	}, nil
}

// deriveCubeRouterSpecFromSandboxCIDR derives fallback router addresses from the
// sandbox CIDR tail. This mode avoids adding a routed prefix and instead installs
// a host route for the NAT IP.
func deriveCubeRouterSpecFromSandboxCIDR(cidr, macAddr string) (*CubeRouterSpec, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox cidr %q: %w", cidr, err)
	}
	if ip.To4() == nil || network.IP.To4() == nil {
		return nil, fmt.Errorf("sandbox cidr %q is not IPv4", cidr)
	}
	mask, bits := network.Mask.Size()
	if bits != 32 || mask < sandboxCIDRMinMask || mask > sandboxCIDRMaxMask {
		return nil, fmt.Errorf("sandbox cidr %q must be between /%d and /%d when cube-router cidr is omitted", cidr, sandboxCIDRMinMask, sandboxCIDRMaxMask)
	}
	if _, err := net.ParseMAC(macAddr); err != nil {
		return nil, fmt.Errorf("parse cube-router mac %q: %w", macAddr, err)
	}

	base := ipv4ToUint32(network.IP)
	size := uint32(1) << (32 - mask)
	return &CubeRouterSpec{
		IP:           uint32ToIPv4(base + size - 3),
		Mask:         32,
		Mac:          macAddr,
		NATIP:        uint32ToIPv4(base + size - 2),
		RoutedPrefix: false,
	}, nil
}

// ipv4ToUint32 converts IPv4 bytes into a host-order integer for address math.
func ipv4ToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	return uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
}

// uint32ToIPv4 converts an address integer back to net.IP.
func uint32ToIPv4(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}

func validateCubeRouterSpec(spec *CubeRouterSpec) error {
	if spec == nil {
		return fmt.Errorf("cube-router spec is nil")
	}
	if spec.IP == nil || spec.IP.To4() == nil {
		return fmt.Errorf("cube-router ip is not an IPv4 address")
	}
	if spec.NATIP == nil || spec.NATIP.To4() == nil {
		return fmt.Errorf("cube-router nat ip is not an IPv4 address")
	}
	if spec.IP.Equal(spec.NATIP) {
		return fmt.Errorf("cube-router nat ip %s must differ from cube-router local ip", spec.NATIP.String())
	}
	if spec.Mask <= 0 || spec.Mask > 32 {
		return fmt.Errorf("invalid cube-router mask %d", spec.Mask)
	}
	if spec.RoutedPrefix && !ipInSameIPv4Prefix(spec.IP, spec.NATIP, spec.Mask) {
		return fmt.Errorf("cube-router nat ip %s is not in %s/%d", spec.NATIP.String(), spec.IP.String(), spec.Mask)
	}
	return ensureIPv4IsNotLocal(spec.NATIP)
}

func cubeRouterAddr(spec *CubeRouterSpec) *netlink.Addr {
	return &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   spec.IP,
			Mask: net.CIDRMask(spec.Mask, 32),
		},
	}
}

func reconcileCubeRouter(dummy *netlink.Dummy, spec *CubeRouterSpec, desiredAddr *netlink.Addr, mtu int) (*CubeRouter, error) {
	if spec.RoutedPrefix {
		if err := ensureCIDRDoesNotOverlapHostRoutes(desiredAddr.IPNet, dummy.Index); err != nil {
			return nil, err
		}
	}
	if err := ensureCubeRouterMAC(dummy, spec.Mac); err != nil {
		return nil, err
	}
	if err := syncCubeRouterAddress(dummy, spec, desiredAddr); err != nil {
		return nil, err
	}
	if dummy.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(dummy); err != nil {
			return nil, err
		}
	}
	if dummy.Attrs().MTU != mtu {
		if err := netlink.LinkSetMTU(dummy, mtu); err != nil {
			return nil, err
		}
	}
	return cubeRouterFromDummy(dummy, spec), nil
}

func syncCubeRouterAddress(dummy *netlink.Dummy, spec *CubeRouterSpec, desiredAddr *netlink.Addr) error {
	addrs, err := netlinkAddrList(dummy, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	hasDesiredAddr := false
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.IP.Equal(spec.IP) {
			ones, _ := addr.IPNet.Mask.Size()
			if ones == spec.Mask {
				hasDesiredAddr = true
				continue
			}
		}
		if err := netlink.AddrDel(dummy, &addr); err != nil {
			return err
		}
	}
	if hasDesiredAddr {
		return nil
	}
	if err := netlink.AddrAdd(dummy, desiredAddr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}

func createCubeRouter(spec *CubeRouterSpec, desiredAddr *netlink.Addr, mtu int) (*CubeRouter, error) {
	mac, err := net.ParseMAC(spec.Mac)
	if err != nil {
		return nil, err
	}
	if spec.RoutedPrefix {
		if err := ensureCIDRDoesNotOverlapHostRoutes(desiredAddr.IPNet, 0); err != nil {
			return nil, err
		}
	}
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         cubeRouterName,
			HardwareAddr: mac,
			TxQLen:       txQLen,
		},
	}
	if err := netlink.LinkAdd(dummy); err != nil {
		return nil, err
	}
	if err := netlink.AddrAdd(dummy, desiredAddr); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(dummy); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetMTU(dummy, mtu); err != nil {
		return nil, err
	}
	return cubeRouterFromDummy(dummy, spec), nil
}

func cubeRouterFromDummy(dummy *netlink.Dummy, spec *CubeRouterSpec) *CubeRouter {
	return &CubeRouter{
		Index:        dummy.Index,
		Name:         cubeRouterName,
		IP:           spec.IP,
		Mask:         spec.Mask,
		Mac:          dummy.HardwareAddr,
		NATIP:        spec.NATIP,
		RoutedPrefix: spec.RoutedPrefix,
	}
}

// ensureCubeRouterMAC rejects an existing cube-router with a different MAC
// because changing it in place would invalidate datapath expectations.
func ensureCubeRouterMAC(dummy *netlink.Dummy, macAddr string) error {
	want, err := net.ParseMAC(macAddr)
	if err != nil {
		return err
	}
	if dummy.HardwareAddr.String() == want.String() {
		return nil
	}
	return fmt.Errorf("%s has MAC %s, want %s", cubeRouterName, dummy.HardwareAddr.String(), want.String())
}

// currentCubeRouter inspects the existing cube-router device, if any, and
// reconstructs enough metadata to compare against the desired spec or clean it.
func currentCubeRouter() (*CubeRouter, error) {
	link, err := netlinkLinkByName(cubeRouterName)
	if err != nil {
		if isLinkNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	dummy, ok := link.(*netlink.Dummy)
	if !ok {
		return nil, fmt.Errorf("%s is not dummy", cubeRouterName)
	}
	router := &CubeRouter{
		Index: dummy.Index,
		Name:  dummy.Name,
		Mac:   dummy.HardwareAddr,
	}
	addrs, err := netlinkAddrList(dummy, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if addr.IPNet == nil || addr.IP.To4() == nil {
			continue
		}
		mask, bits := addr.IPNet.Mask.Size()
		if bits != 32 || mask <= 0 || mask > 32 {
			continue
		}
		router.IP = addr.IP.To4()
		router.Mask = mask
		if mask <= 30 {
			router.NATIP = uint32ToIPv4(ipv4ToUint32(addr.IP.Mask(addr.IPNet.Mask)) + 2)
			router.RoutedPrefix = true
		} else if mask == 32 {
			router.NATIP = uint32ToIPv4(ipv4ToUint32(addr.IP) + 1)
		}
		return router, nil
	}
	return router, nil
}

// isLinkNotFound normalizes netlink's concrete not-found error and older string
// forms returned by some netlink paths.
func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}

// ipInSameIPv4Prefix reports whether ip belongs to base/mask.
func ipInSameIPv4Prefix(base, ip net.IP, mask int) bool {
	base4 := base.To4()
	ip4 := ip.To4()
	if base4 == nil || ip4 == nil {
		return false
	}
	return (&net.IPNet{IP: base4, Mask: net.CIDRMask(mask, 32)}).Contains(ip4)
}

// ensureIPv4IsNotLocal rejects a NAT IP that is already configured on any host
// link, which would make CubeVS egress ambiguous or break local routing.
func ensureIPv4IsNotLocal(ip net.IP) error {
	links, err := netlinkLinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		addrs, err := netlinkAddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				return fmt.Errorf("cube-router nat ip %s must not be configured as local address on %s", ip.String(), link.Attrs().Name)
			}
		}
	}
	return nil
}

// ensureCIDRDoesNotOverlapHostRoutes protects host routing by rejecting a
// cube-router prefix that overlaps an existing IPv4 route on another interface.
func ensureCIDRDoesNotOverlapHostRoutes(cidr *net.IPNet, ignoreLinkIndex int) error {
	if cidr == nil || cidr.IP.To4() == nil {
		return fmt.Errorf("cube-router prefix is not an IPv4 CIDR")
	}
	links, err := netlinkLinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		routes, err := netlinkRouteList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst == nil || route.Dst.IP.To4() == nil {
				continue
			}
			if ignoreLinkIndex != 0 && route.LinkIndex == ignoreLinkIndex {
				continue
			}
			ones, bits := route.Dst.Mask.Size()
			if bits != 32 || ones == 0 {
				continue
			}
			if cidrsOverlap(cidr, route.Dst) {
				return fmt.Errorf("cube-router prefix %s overlaps host route %s on %s", cidr.String(), route.Dst.String(), link.Attrs().Name)
			}
		}
	}
	return nil
}

// cidrsOverlap reports whether two IPv4 CIDRs intersect.
func cidrsOverlap(a, b *net.IPNet) bool {
	if a == nil || b == nil || a.IP.To4() == nil || b.IP.To4() == nil {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// deleteCubeRouterHostNetworking removes the host-level side effects installed
// for cube-router before the dummy link itself is deleted.
func deleteCubeRouterHostNetworking(router *CubeRouter, snatPortMin uint16) error {
	if router == nil || router.IP == nil || router.NATIP == nil {
		return nil
	}
	if err := deleteCubeRouterIptables(router, snatPortMin); err != nil {
		return err
	}
	if !router.RoutedPrefix {
		if err := deleteRouteToCubeRouterNAT(router); err != nil {
			return err
		}
	}
	_ = netlink.NeighDel(&netlink.Neigh{
		Family:    netlink.FAMILY_V4,
		IP:        router.NATIP,
		LinkIndex: router.Index,
	})
	return nil
}

// ensureCubeRouterNATNeighbor installs a permanent neighbor entry for the NAT IP
// on cube-router so packets redirected there resolve locally without ARP.
func ensureCubeRouterNATNeighbor(router *CubeRouter) error {
	return netlink.NeighSet(&netlink.Neigh{
		Family:       netlink.FAMILY_V4,
		IP:           router.NATIP,
		HardwareAddr: router.Mac,
		LinkIndex:    router.Index,
		State:        netlink.NUD_PERMANENT,
	})
}

// ensureRouteToCubeRouterNAT installs the fallback /32 route used when no
// explicit cube-router CIDR was configured.
func ensureRouteToCubeRouterNAT(router *CubeRouter) error {
	if router == nil || router.Index == 0 || router.NATIP == nil {
		return fmt.Errorf("cube-router is not initialized")
	}
	dst := &net.IPNet{IP: router.NATIP, Mask: net.CIDRMask(32, 32)}
	route := &netlink.Route{
		LinkIndex: router.Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_STATIC,
	}
	routes, err := netlinkRouteListFiltered(netlink.FAMILY_V4, route, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("list route for %s via %s: %w", dst.String(), router.Name, err)
	}
	for _, existing := range routes {
		if existing.Dst != nil && existing.Dst.String() == dst.String() && existing.LinkIndex == router.Index {
			return nil
		}
	}
	return netlinkRouteReplace(route)
}

// deleteRouteToCubeRouterNAT removes the fallback /32 route if it exists.
func deleteRouteToCubeRouterNAT(router *CubeRouter) error {
	if router == nil || router.Index == 0 || router.NATIP == nil {
		return nil
	}
	err := netlinkRouteDel(&netlink.Route{
		LinkIndex: router.Index,
		Dst:       &net.IPNet{IP: router.NATIP, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_STATIC,
	})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such") {
		return err
	}
	return nil
}

// ensureCubeRouterIptables installs forwarding and MASQUERADE rules required for
// route-aware egress.
func ensureCubeRouterIptables(router *CubeRouter, snatPortMin uint16) error {
	// iptables -t filter -A FORWARD -i <cube-router> -s <nat-ip>/32 -j ACCEPT
	// Allows sandbox egress packets that have been normalized by CubeVS to
	// router.NATIP to enter the host forwarding path from cube-router.
	if err := runIptablesEnsure("-t", "filter", "-A", "FORWARD",
		"-i", router.Name,
		"-s", router.NATIP.String()+"/32",
		"-j", "ACCEPT"); err != nil {
		return err
	}

	// iptables -t filter -A FORWARD -o <cube-router> -d <nat-ip>/32 \
	//   -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
	// Allows return traffic that conntrack has already associated with
	// cube-router egress sessions to be forwarded back toward cube-router.
	if err := runIptablesEnsure("-t", "filter", "-A", "FORWARD",
		"-o", router.Name,
		"-d", router.NATIP.String()+"/32",
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
		"-j", "ACCEPT"); err != nil {
		return err
	}

	for _, rule := range cubeRouterMasqueradeRules(router, snatPortMin) {
		if err := runIptablesEnsure(rule...); err != nil {
			return err
		}
	}
	return nil
}

// deleteCubeRouterIptables removes the rules installed by ensureCubeRouterIptables.
func deleteCubeRouterIptables(router *CubeRouter, snatPortMin uint16) error {
	rules := [][]string{
		{"-t", "filter", "-A", "FORWARD",
			"-i", router.Name,
			"-s", router.NATIP.String() + "/32",
			"-j", "ACCEPT"},
		{"-t", "filter", "-A", "FORWARD",
			"-o", router.Name,
			"-d", router.NATIP.String() + "/32",
			"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
			"-j", "ACCEPT"},
	}
	rules = append(rules, cubeRouterMasqueradeRules(router, snatPortMin)...)
	for _, rule := range rules {
		if err := runIptablesDeleteIfExists(rule...); err != nil {
			return err
		}
	}
	return nil
}

func cubeRouterMasqueradeRules(router *CubeRouter, snatPortMin uint16) [][]string {
	// Common selector for all cube-router SNAT rules:
	//
	// iptables -t nat -A POSTROUTING -s <nat-ip>/32 ! -o <cube-router> ...
	//
	// Only packets that came from CubeVS's internal NAT IP and are leaving via
	// a real host-selected egress device should be MASQUERADE'd. Packets whose
	// output device is cube-router are excluded to avoid rewriting traffic that
	// is being delivered back toward sandboxes.
	base := []string{
		"-t", "nat", "-A", "POSTROUTING",
		"-s", router.NATIP.String() + "/32",
		"!", "-o", router.Name,
	}

	// iptables -t nat -A POSTROUTING -s <nat-ip>/32 ! -o <cube-router> \
	//   -p tcp -j MASQUERADE --to-ports <port-mapping-max+1>-65535
	// Rewrites TCP egress to the selected host egress IP and constrains the
	// ephemeral source port range so it does not collide with CubeVS port
	// mapping ports.
	tcpRule := append(append([]string{}, base...), "-p", "tcp")
	tcpRule = append(tcpRule,
		"-j", "MASQUERADE",
		"--to-ports", fmt.Sprintf("%d-%d", snatPortMin, tcpPortMax))

	// iptables -t nat -A POSTROUTING -s <nat-ip>/32 ! -o <cube-router> \
	//   -p udp -j MASQUERADE
	// Rewrites UDP egress to the selected host egress IP. UDP has no CubeVS
	// port-mapping collision concern today, so no explicit --to-ports is used.
	udpRule := append(append([]string{}, base...), "-p", "udp")
	udpRule = append(udpRule, "-j", "MASQUERADE")

	// iptables -t nat -A POSTROUTING -s <nat-ip>/32 ! -o <cube-router> \
	//   -p icmp -j MASQUERADE
	// Rewrites ICMP egress to the selected host egress IP so ping and similar
	// diagnostics follow the same route-aware path.
	icmpRule := append(append([]string{}, base...), "-p", "icmp")
	icmpRule = append(icmpRule, "-j", "MASQUERADE")

	return [][]string{tcpRule, udpRule, icmpRule}
}

// runIptablesEnsure implements idempotent "check, then append" for one iptables rule.
func runIptablesEnsure(args ...string) error {
	checkArgs, err := iptablesArgsWithAction(args, "-C")
	if err != nil {
		return err
	}
	if err := execCommand("iptables", checkArgs...).Run(); err == nil {
		return nil
	}
	out, err := execCommand("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runIptablesDeleteIfExists implements idempotent deletion for one iptables rule.
func runIptablesDeleteIfExists(args ...string) error {
	checkArgs, err := iptablesArgsWithAction(args, "-C")
	if err != nil {
		return err
	}
	if err := execCommand("iptables", checkArgs...).Run(); err != nil {
		return nil
	}

	deleteArgs, err := iptablesArgsWithAction(args, "-D")
	if err != nil {
		return err
	}
	out, err := execCommand("iptables", deleteArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s failed: %w: %s",
			strings.Join(deleteArgs, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// iptablesArgsWithAction rewrites a rule containing -A into the corresponding
// -C or -D command form.
func iptablesArgsWithAction(args []string, action string) ([]string, error) {
	out := append([]string(nil), args...)
	for i, arg := range out {
		if arg == "-A" {
			out[i] = action
			return out, nil
		}
	}
	return nil, fmt.Errorf("iptables rule is missing -A action: %s", strings.Join(args, " "))
}

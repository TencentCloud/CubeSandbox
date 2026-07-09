// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

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

var (
	netlinkAddrList = netlink.AddrList
	netlinkRouteDel = netlink.RouteDel
	execCommand     = exec.Command
)

const (
	cubeRouterName = "cube-router"
)

type cubeRouter struct {
	Index        int
	Name         string
	IP           net.IP
	Mask         int
	Mac          net.HardwareAddr
	NATIP        net.IP
	RoutedPrefix bool
}

type cubeRouterSpec struct {
	IP           net.IP
	Mask         int
	Mac          string
	NATIP        net.IP
	RoutedPrefix bool
}

func deriveCubeRouterCIDRSpec(cidr, macAddr string) (*cubeRouterSpec, error) {
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
	return &cubeRouterSpec{
		IP:           uint32ToIPv4(base + 1),
		Mask:         mask,
		Mac:          macAddr,
		NATIP:        uint32ToIPv4(base + 2),
		RoutedPrefix: true,
	}, nil
}

func deriveCubeRouterSpecFromSandboxCIDR(cidr, macAddr string) (*cubeRouterSpec, error) {
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
	return &cubeRouterSpec{
		IP:           uint32ToIPv4(base + size - 3),
		Mask:         32,
		Mac:          macAddr,
		NATIP:        uint32ToIPv4(base + size - 2),
		RoutedPrefix: false,
	}, nil
}

func cubeRouterSpecFromConfig(cfg Config) (*cubeRouterSpec, error) {
	if cfg.CubeRouterCIDR != "" {
		return deriveCubeRouterCIDRSpec(cfg.CubeRouterCIDR, cfg.CubeRouterMacAddr)
	}
	return deriveCubeRouterSpecFromSandboxCIDR(cfg.CIDR, cfg.CubeRouterMacAddr)
}

func ipv4ToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	return uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
}

func uint32ToIPv4(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}

func getOrCreateCubeRouter(spec *cubeRouterSpec, mtu int) (*cubeRouter, error) {
	if spec == nil {
		return nil, fmt.Errorf("cube-router spec is nil")
	}
	ip := spec.IP
	mask := spec.Mask
	natIP := spec.NATIP
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("cube-router ip is not an IPv4 address")
	}
	if natIP == nil || natIP.To4() == nil {
		return nil, fmt.Errorf("cube-router nat ip is not an IPv4 address")
	}
	if ip.Equal(natIP) {
		return nil, fmt.Errorf("cube-router nat ip %s must differ from cube-router local ip", natIP.String())
	}
	if mask <= 0 || mask > 32 {
		return nil, fmt.Errorf("invalid cube-router mask %d", mask)
	}
	if spec.RoutedPrefix && !ipInSameIPv4Prefix(ip, natIP, mask) {
		return nil, fmt.Errorf("cube-router nat ip %s is not in %s/%d", natIP.String(), ip.String(), mask)
	}
	if err := ensureIPv4IsNotLocal(natIP); err != nil {
		return nil, err
	}

	desiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(mask, 32),
		},
	}
	link, err := netlinkLinkByName(cubeRouterName)
	if err == nil {
		dummy, ok := link.(*netlink.Dummy)
		if !ok {
			return nil, fmt.Errorf("%s is not dummy", cubeRouterName)
		}
		if spec.RoutedPrefix {
			if err := ensureCIDRDoesNotOverlapHostRoutes(desiredAddr.IPNet, dummy.Index); err != nil {
				return nil, err
			}
		}
		if err := ensureCubeRouterMAC(dummy, spec.Mac); err != nil {
			return nil, err
		}
		addrs, err := netlinkAddrList(dummy, netlink.FAMILY_V4)
		if err != nil {
			return nil, err
		}
		hasDesiredAddr := false
		for _, addr := range addrs {
			if addr.IPNet != nil && addr.IPNet.IP.Equal(ip) {
				ones, _ := addr.IPNet.Mask.Size()
				if ones == mask {
					hasDesiredAddr = true
					continue
				}
			}
			if err := netlink.AddrDel(dummy, &addr); err != nil {
				return nil, err
			}
		}
		if !hasDesiredAddr {
			if err := netlink.AddrAdd(dummy, desiredAddr); err != nil && !errors.Is(err, syscall.EEXIST) {
				return nil, err
			}
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
		return &cubeRouter{
			Index:        dummy.Index,
			Name:         cubeRouterName,
			IP:           ip,
			Mask:         mask,
			Mac:          dummy.HardwareAddr,
			NATIP:        natIP,
			RoutedPrefix: spec.RoutedPrefix,
		}, nil
	}
	if !isLinkNotFound(err) {
		return nil, fmt.Errorf("lookup %s: %w", cubeRouterName, err)
	}
	gwAddr, err := net.ParseMAC(spec.Mac)
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
			HardwareAddr: gwAddr,
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
	return &cubeRouter{
		Index:        dummy.Index,
		Name:         cubeRouterName,
		IP:           ip,
		Mask:         mask,
		Mac:          dummy.HardwareAddr,
		NATIP:        natIP,
		RoutedPrefix: spec.RoutedPrefix,
	}, nil
}

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

func ensureCubeRouterMatches(spec *cubeRouterSpec) error {
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
	return cleanupCubeRouter()
}

func currentCubeRouter() (*cubeRouter, error) {
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
	router := &cubeRouter{
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

func cleanupCubeRouter() error {
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
		if err := deleteCubeRouterHostNetworking(router); err != nil {
			return err
		}
	}
	return netlinkLinkDel(link)
}

func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.As(err, &notFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}

func ipInSameIPv4Prefix(base, ip net.IP, mask int) bool {
	base4 := base.To4()
	ip4 := ip.To4()
	if base4 == nil || ip4 == nil {
		return false
	}
	return (&net.IPNet{IP: base4, Mask: net.CIDRMask(mask, 32)}).Contains(ip4)
}

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

func cidrsOverlap(a, b *net.IPNet) bool {
	if a == nil || b == nil || a.IP.To4() == nil || b.IP.To4() == nil {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func configureCubeRouterHostNetworking(router *cubeRouter) error {
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
	if err := ensureCubeRouterIptables(router); err != nil {
		return err
	}
	if err := ensureCubeRouterNATNeighbor(router); err != nil {
		return err
	}
	return nil
}

func deleteCubeRouterHostNetworking(router *cubeRouter) error {
	if router == nil || router.IP == nil || router.NATIP == nil {
		return nil
	}
	if err := deleteCubeRouterIptables(router); err != nil {
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

func ensureCubeRouterNATNeighbor(router *cubeRouter) error {
	return netlink.NeighSet(&netlink.Neigh{
		Family:       netlink.FAMILY_V4,
		IP:           router.NATIP,
		HardwareAddr: router.Mac,
		LinkIndex:    router.Index,
		State:        netlink.NUD_PERMANENT,
	})
}

func ensureRouteToCubeRouterNAT(router *cubeRouter) error {
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

func deleteRouteToCubeRouterNAT(router *cubeRouter) error {
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

func ensureCubeRouterIptables(router *cubeRouter) error {
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

	for _, rule := range cubeRouterMasqueradeRules(router) {
		if err := runIptablesEnsure(rule...); err != nil {
			return err
		}
	}
	return nil
}

func deleteCubeRouterIptables(router *cubeRouter) error {
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
	rules = append(rules, cubeRouterMasqueradeRules(router)...)
	for _, rule := range rules {
		if err := runIptablesDeleteIfExists(rule...); err != nil {
			return err
		}
	}
	return nil
}

func cubeRouterMasqueradeRules(router *cubeRouter) [][]string {
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

	snatPortMin, snatPortMax := cubeSNATPortRange()

	// iptables -t nat -A POSTROUTING -s <nat-ip>/32 ! -o <cube-router> \
	//   -p tcp -j MASQUERADE --to-ports <port-mapping-max+1>-65535
	// Rewrites TCP egress to the selected host egress IP and constrains the
	// ephemeral source port range so it does not collide with CubeVS port
	// mapping ports.
	tcpRule := append(append([]string{}, base...), "-p", "tcp")
	tcpRule = append(tcpRule,
		"-j", "MASQUERADE",
		"--to-ports", fmt.Sprintf("%d-%d", snatPortMin, snatPortMax))

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

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// gatewayARPRetries is the number of times GetGatewayMacAddr will re-probe the
// neighbor table after triggering ARP resolution. Node initialization may race
// the kernel's ARP learning, so a single read is not enough.
const gatewayARPRetries = 5

// gatewayARPBackoff is the delay between ARP trigger retries. It gives the
// kernel time to complete the ARP request/response cycle. It is a var so
// tests can set it to zero for fast failure-path execution.
var gatewayARPBackoff = 200 * time.Millisecond

// errGatewayMacNotFound is returned by lookupGatewayMac when the neighbor
// table was read successfully but no usable entry for the gateway IP exists.
// Callers can distinguish this from real netlink errors via errors.Is.
var errGatewayMacNotFound = errors.New("gateway mac not found in neighbor table")

// maxDumpRetries is higher than containernetworking/plugins netlinksafe (5)
// because Cubelet density creates can keep the link table mutating for longer
// than a single LinkList of hundreds of TAPs takes to complete.
const maxDumpRetries = 16

// dumpRetryBackoff is a short pause between interrupted dumps so concurrent
// LinkAdd/LinkDel traffic can settle before the next attempt.
const dumpRetryBackoff = 2 * time.Millisecond

var (
	// Package-level function variables are test seams for host networking helpers.
	// Dump-style reads go through WithDumpRetry so NLM_F_DUMP_INTR under TAP
	// churn is retried; mutating calls are left unwrapped.
	netlinkRouteReplace      = netlink.RouteReplace
	netlinkRouteListFiltered = func(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
		return WithDumpRetry(func() ([]netlink.Route, error) {
			return netlink.RouteListFiltered(family, filter, mask)
		})
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return WithDumpRetry(func() ([]netlink.Route, error) {
			return netlink.RouteList(link, family)
		})
	}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return WithDumpRetry(func() (netlink.Link, error) {
			return netlink.LinkByName(name)
		})
	}
	netlinkLinkList = func() ([]netlink.Link, error) {
		return WithDumpRetry(netlink.LinkList)
	}
	netlinkLinkDel   = netlink.LinkDel
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return WithDumpRetry(func() ([]netlink.Neigh, error) {
			return netlink.NeighList(linkIndex, family)
		})
	}
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return WithDumpRetry(func() ([]netlink.Addr, error) {
			return netlink.AddrList(link, family)
		})
	}
	netlinkRouteDel = netlink.RouteDel
	netlinkNeighSet = netlink.NeighSet

	// sendUDPProbe sends a small UDP packet to gatewayIP from localIP, forcing
	// the kernel to create a neighbor entry and issue an ARP request through
	// the network stack. Binding to localIP ensures the packet egresses via
	// the correct interface so the ARP entry lands in the neighbor table that
	// lookupGatewayMac reads. It is a var so tests can replace it to avoid
	// live network I/O. Returns an error when the probe socket cannot be
	// created or the datagram fails to send, so callers can distinguish
	// "probe never fired" from "ARP genuinely unanswered".
	sendUDPProbe = func(gatewayIP net.IP, localIP net.IP) error {
		var localAddr *net.UDPAddr
		if localIP != nil {
			localAddr = &net.UDPAddr{IP: localIP}
		}
		// Port 9 (discard) is used instead of a well-known service port to
		// avoid triggering IDS alerts or ICMP port-unreachable storms that
		// port 1 (tcpmux) can cause on some networks.
		conn, err := net.DialUDP("udp4", localAddr, &net.UDPAddr{IP: gatewayIP, Port: 9})
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0}); err != nil {
			return fmt.Errorf("udp probe write to %s: %w", gatewayIP, err)
		}
		return nil
	}

	// triggerARPResolution forces the kernel to resolve the gateway's L2
	// address. NeighSet(NUD_PROBE) re-probes an existing neighbor entry to
	// trigger a fresh ARP request; for the missing-entry case the call may
	// create an empty shell without starting ARP, so the subsequent UDP
	// probe is the primary mechanism that creates a new entry through the
	// full network stack. Any empty entry left by NeighSet is harmless —
	// lookupGatewayMac ignores entries without a hardware address.
	// localIP is the address of the egress interface; binding the UDP probe
	// to it ensures the ARP entry is created on the correct interface.
	triggerARPResolution = func(linkIndex int, gatewayIP net.IP, localIP net.IP) error {
		probeNeighborEntry(linkIndex, gatewayIP)
		return sendUDPProbe(gatewayIP, localIP)
	}
)

// probeNeighborEntry sets NUD_PROBE on an existing neighbor entry for
// gatewayIP, causing the kernel to re-send an ARP request. When no entry
// exists the call may create an empty shell without triggering ARP; the
// caller relies on sendUDPProbe to handle that case. Errors are silently
// ignored because sendUDPProbe always follows as a fallback.
func probeNeighborEntry(linkIndex int, gatewayIP net.IP) {
	_ = netlinkNeighSet(&netlink.Neigh{
		IP:        gatewayIP,
		LinkIndex: linkIndex,
		State:     unix.NUD_PROBE,
		Family:    netlink.FAMILY_V4,
	})
}

// HostDevice captures the configured host network device selected by Cubelet.
type HostDevice struct {
	Index      int
	Name       string
	IP         net.IP
	IPMask     net.IPMask
	Mac        net.HardwareAddr
	GatewayMac net.HardwareAddr
}

// GetHostDevice validates the configured host interface and captures the
// addresses CubeVS needs for SNAT and L2 forwarding.
func GetHostDevice(ifName string) (*HostDevice, error) {
	link, err := netlinkLinkByName(ifName)
	if err != nil {
		return nil, err
	}
	addrs, err := netlinkAddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	if len(addrs) != 1 {
		return nil, fmt.Errorf("ipv4 address on %s is not unique", ifName)
	}
	gwMac, err := GetGatewayMacAddr(ifName)
	if err != nil {
		return nil, err
	}
	gatewayMac, err := net.ParseMAC(gwMac)
	if err != nil {
		return nil, err
	}
	return &HostDevice{
		Index:      link.Attrs().Index,
		Name:       link.Attrs().Name,
		IP:         addrs[0].IP,
		IPMask:     addrs[0].Mask,
		Mac:        link.Attrs().HardwareAddr,
		GatewayMac: gatewayMac,
	}, nil
}

// GetGatewayMacAddr resolves the MAC address of the default gateway on ifName
// from the neighbor table. CubeVS needs this L2 destination for direct egress.
//
// Side effects: when the neighbor entry has not been learned yet (common during
// node boot), the function triggers active ARP resolution by sending a UDP
// probe to the gateway (port 9, discard) and retrying with a short backoff.
// On a cache miss it sends up to gatewayARPRetries (5) UDP datagrams and
// blocks for up to gatewayARPRetries × gatewayARPBackoff (~1 s) before giving
// up. There is no side-effect-free path once the initial neighbor-table read
// returns a miss. Real netlink errors (EPERM, ENODEV, dump exhaustion) are
// propagated immediately without probing.
func GetGatewayMacAddr(ifName string) (string, error) {
	link, err := netlinkLinkByName(ifName)
	if err != nil {
		return "", err
	}
	gatewayIP, err := defaultGatewayIP(link)
	if err != nil {
		return "", err
	}

	// First attempt: read whatever the neighbor table already has.
	mac, err := lookupGatewayMac(link.Attrs().Index, gatewayIP)
	if err == nil {
		return mac, nil
	}
	if !errors.Is(err, errGatewayMacNotFound) {
		return "", fmt.Errorf("lookup gateway mac on %s: %w", ifName, err)
	}

	// Cache miss: actively trigger ARP and retry. During node initialization
	// the gateway's neighbor entry may not exist yet; triggerARPResolution
	// first tries NeighSet(NUD_PROBE) then falls back to a UDP probe bound
	// to the interface's local IP so the ARP entry lands on the correct
	// interface's neighbor table.
	// Extract the interface's IPv4 address to bind the UDP probe, ensuring
	// the kernel sends ARP on the same interface lookupGatewayMac reads.
	// Returning the error here is critical: without a bound source address
	// the kernel picks the IP from the route, which on a multi-homed node
	// may egress on a different interface, silently defeating the fix.
	addrs, err := netlinkAddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list addrs on %s for UDP probe: %w", ifName, err)
	}
	var localIP net.IP
	for _, a := range addrs {
		if a.IP.To4() != nil {
			localIP = a.IP
			break
		}
	}

	// Each iteration: trigger → sleep → lookup. Sleeping before the lookup
	// gives the kernel time to complete ARP, so every trigger (including the
	// last) contributes to the resolution budget.
	// NOTE: lastProbeErr only captures the final attempt's probe error.
	// If early probes fail but later ones succeed while ARP stays unresolved,
	// earlier failures are not surfaced in the error message. This is
	// acceptable because all probes use the same interface/IP and failures
	// are typically consistent across attempts; the primary diagnostic
	// (errGatewayMacNotFound) is always returned regardless.
	var lastErr, lastProbeErr error
	for attempt := 0; attempt < gatewayARPRetries; attempt++ {
		lastProbeErr = triggerARPResolution(link.Attrs().Index, gatewayIP, localIP)
		time.Sleep(gatewayARPBackoff)
		mac, lastErr = lookupGatewayMac(link.Attrs().Index, gatewayIP)
		if lastErr == nil {
			return mac, nil
		}
		if !errors.Is(lastErr, errGatewayMacNotFound) {
			return "", fmt.Errorf("lookup gateway mac on %s: %w", ifName, lastErr)
		}
	}
	if lastProbeErr != nil {
		return "", fmt.Errorf("gateway mac for %s via %s not found after %d retries (last probe error: %v): %w",
			ifName, gatewayIP.String(), gatewayARPRetries, lastProbeErr, lastErr)
	}
	return "", fmt.Errorf("gateway mac for %s via %s not found after %d retries: %w",
		ifName, gatewayIP.String(), gatewayARPRetries, lastErr)
}

// lookupGatewayMac scans the neighbor table on linkIndex for a usable entry
// matching gatewayIP. Returns errGatewayMacNotFound when the table was read
// successfully but no matching entry exists; real netlink errors are returned
// as-is so callers can distinguish them via errors.Is.
func lookupGatewayMac(linkIndex int, gatewayIP net.IP) (string, error) {
	neighs, err := netlinkNeighList(linkIndex, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}
	for _, neigh := range neighs {
		if isUsableGatewayNeighbor(neigh, gatewayIP) {
			return neigh.HardwareAddr.String(), nil
		}
	}
	return "", errGatewayMacNotFound
}

// defaultGatewayIP chooses the lowest-metric IPv4 default route on link.
func defaultGatewayIP(link netlink.Link) (net.IP, error) {
	routes, err := netlinkRouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var gatewayIP net.IP
	var gatewayMetric int
	for _, route := range routes {
		if !isIPv4DefaultRoute(route.Dst) || route.Gw.To4() == nil {
			continue
		}
		if gatewayIP == nil || route.Priority < gatewayMetric {
			gatewayIP = route.Gw.To4()
			gatewayMetric = route.Priority
		}
	}
	if gatewayIP == nil {
		return nil, fmt.Errorf("default gateway not found on %s", link.Attrs().Name)
	}
	return gatewayIP, nil
}

// isIPv4DefaultRoute reports whether dst represents 0.0.0.0/0.
func isIPv4DefaultRoute(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, bits := dst.Mask.Size()
	return bits == 32 && ones == 0
}

// isUsableGatewayNeighbor accepts reachable or recoverable neighbor states for
// the selected gateway. Failed/incomplete entries are ignored.
func isUsableGatewayNeighbor(neigh netlink.Neigh, gatewayIP net.IP) bool {
	if neigh.Family != netlink.FAMILY_V4 || !neigh.IP.Equal(gatewayIP) || len(neigh.HardwareAddr) == 0 {
		return false
	}
	switch neigh.State {
	case unix.NUD_REACHABLE, unix.NUD_STALE, unix.NUD_DELAY, unix.NUD_PROBE, unix.NUD_PERMANENT:
		return true
	default:
		return false
	}
}

// WithDumpRetry runs op, retrying when a netlink dump was interrupted because
// the table changed mid-read (ErrDumpInterrupted / EINTR). Other errors and
// success return immediately. Mutating netlink calls should not use this.
func WithDumpRetry[T any](op func() (T, error)) (T, error) {
	var (
		zero T
		last error
	)
	for attempt := 0; attempt < maxDumpRetries; attempt++ {
		v, err := op()
		if err == nil || !isDumpInterrupted(err) {
			return v, err
		}
		last = err
		if attempt+1 < maxDumpRetries {
			time.Sleep(dumpRetryBackoff)
		}
	}
	return zero, last
}

func isDumpInterrupted(err error) bool {
	return errors.Is(err, netlink.ErrDumpInterrupted) || errors.Is(err, unix.EINTR)
}

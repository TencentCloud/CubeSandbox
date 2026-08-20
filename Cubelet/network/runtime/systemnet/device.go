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
)

// HostDevice captures the configured host network device selected by Cubelet.
type HostDevice struct {
	Index      int
	Name       string
	IP         net.IP
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
		Mac:        link.Attrs().HardwareAddr,
		GatewayMac: gatewayMac,
	}, nil
}

// GetGatewayMacAddr resolves the MAC address of the default gateway on ifName
// from the neighbor table. CubeVS needs this L2 destination for direct egress.
func GetGatewayMacAddr(ifName string) (string, error) {
	link, err := netlinkLinkByName(ifName)
	if err != nil {
		return "", err
	}
	gatewayIP, err := defaultGatewayIP(link)
	if err != nil {
		return "", err
	}
	neighs, err := netlinkNeighList(link.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}
	for _, neigh := range neighs {
		if isUsableGatewayNeighbor(neigh, gatewayIP) {
			return neigh.HardwareAddr.String(), nil
		}
	}
	return "", fmt.Errorf("gateway mac for %s via %s not found", ifName, gatewayIP.String())
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

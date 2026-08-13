// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var (
	// Package-level function variables are test seams for host networking helpers.
	netlinkRouteReplace      = netlink.RouteReplace
	netlinkRouteListFiltered = netlink.RouteListFiltered
	netlinkRouteList         = netlink.RouteList
	netlinkLinkByName        = netlink.LinkByName
	netlinkLinkList          = netlink.LinkList
	netlinkLinkDel           = netlink.LinkDel
	netlinkNeighList         = netlink.NeighList
	netlinkAddrList          = netlink.AddrList
	netlinkRouteDel          = netlink.RouteDel
)

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
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
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

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	cubeDevName = "cube-dev"
	txQLen      = 1000
)

// CubeDev is the dummy gateway device that owns the sandbox CIDR route.
type CubeDev struct {
	Index int
	Name  string
	IP    net.IP
	Mac   net.HardwareAddr
}

// GetOrCreateCubeDev creates or reconciles the cube-dev dummy link. The device
// provides the host-side gateway for sandbox TAP addresses and owns the sandbox
// CIDR route.
func GetOrCreateCubeDev(ip net.IP, mask, mtu int, macAddr string) (*CubeDev, error) {
	desiredAddr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(mask, 32),
		},
	}
	link, err := netlinkLinkByName(cubeDevName)
	if err == nil {
		dummy, ok := link.(*netlink.Dummy)
		if !ok {
			return nil, fmt.Errorf("%s is not dummy", cubeDevName)
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
		return &CubeDev{
			Index: dummy.Index,
			Name:  cubeDevName,
			IP:    ip,
			Mac:   dummy.HardwareAddr,
		}, nil
	}
	gwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return nil, err
	}
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         cubeDevName,
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
	return &CubeDev{
		Index: dummy.Index,
		Name:  cubeDevName,
		IP:    ip,
		Mac:   dummy.HardwareAddr,
	}, nil
}

// AddARPEntry installs a permanent neighbor entry so sandbox traffic targeting
// the mvm gateway MAC resolves through cube-dev without ARP churn.
func AddARPEntry(ip net.IP, mac string, cubeDevIndex int) error {
	macAddr, err := net.ParseMAC(mac)
	if err != nil {
		return err
	}
	return netlink.NeighSet(&netlink.Neigh{
		Family:       netlink.FAMILY_V4,
		IP:           ip,
		HardwareAddr: macAddr,
		LinkIndex:    cubeDevIndex,
		State:        unix.NUD_PERMANENT,
		Type:         unix.RTN_UNSPEC,
	})
}

// EnsureRouteToCubeDev installs the sandbox CIDR route through cube-dev if it
// is not already present.
func EnsureRouteToCubeDev(cidr string, dev *CubeDev) error {
	if dev == nil || dev.Index == 0 {
		return fmt.Errorf("cube-dev is not initialized")
	}
	_, dst, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse mvm cidr %q: %w", cidr, err)
	}
	filter := &netlink.Route{
		LinkIndex: dev.Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_STATIC,
	}
	routes, err := netlinkRouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("list route for %s via %s: %w", dst.String(), dev.Name, err)
	}
	for _, route := range routes {
		if route.Dst != nil && route.Dst.String() == dst.String() && route.LinkIndex == dev.Index {
			return nil
		}
	}
	return netlinkRouteReplace(filter)
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestEnsureRouteToCubeDev(t *testing.T) {
	originalReplace := netlinkRouteReplace
	originalList := netlinkRouteListFiltered
	defer func() {
		netlinkRouteReplace = originalReplace
		netlinkRouteListFiltered = originalList
	}()

	var got *netlink.Route
	netlinkRouteListFiltered = func(_ int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return nil, nil
	}
	netlinkRouteReplace = func(route *netlink.Route) error {
		got = route
		return nil
	}

	err := ensureRouteToCubeDev("192.168.0.0/18", &cubeDev{
		Index: 7,
		Name:  cubeDevName,
		IP:    net.ParseIP("192.168.0.1").To4(),
	})
	if err != nil {
		t.Fatalf("ensureRouteToCubeDev error=%v", err)
	}
	if got == nil {
		t.Fatal("route=nil, want route to be installed")
	}
	if got.LinkIndex != 7 {
		t.Fatalf("LinkIndex=%d, want 7", got.LinkIndex)
	}
	if got.Dst == nil || got.Dst.String() != "192.168.0.0/18" {
		t.Fatalf("Dst=%v, want 192.168.0.0/18", got.Dst)
	}
	if got.Scope != netlink.SCOPE_LINK {
		t.Fatalf("Scope=%d, want %d", got.Scope, netlink.SCOPE_LINK)
	}
	if got.Protocol != unix.RTPROT_STATIC {
		t.Fatalf("Protocol=%d, want %d", got.Protocol, unix.RTPROT_STATIC)
	}
}

func TestEnsureRouteToCubeDevSkipsExistingRoute(t *testing.T) {
	originalReplace := netlinkRouteReplace
	originalList := netlinkRouteListFiltered
	defer func() {
		netlinkRouteReplace = originalReplace
		netlinkRouteListFiltered = originalList
	}()

	netlinkRouteListFiltered = func(_ int, route *netlink.Route, _ uint64) ([]netlink.Route, error) {
		return []netlink.Route{{
			LinkIndex: route.LinkIndex,
			Dst:       route.Dst,
			Scope:     route.Scope,
		}}, nil
	}
	netlinkRouteReplace = func(_ *netlink.Route) error {
		t.Fatal("route replace should not be called when route already exists")
		return nil
	}

	err := ensureRouteToCubeDev("192.168.0.0/18", &cubeDev{Index: 7, Name: cubeDevName})
	if err != nil {
		t.Fatalf("ensureRouteToCubeDev error=%v", err)
	}
}

func TestEnsureRouteToCubeDevRejectsInvalidCIDR(t *testing.T) {
	err := ensureRouteToCubeDev("bad-cidr", &cubeDev{Index: 7, Name: cubeDevName})
	if err == nil {
		t.Fatal("ensureRouteToCubeDev error=nil, want invalid cidr")
	}
}

func TestEnsureRouteToCubeDevRequiresDevice(t *testing.T) {
	err := ensureRouteToCubeDev("192.168.0.0/18", nil)
	if err == nil {
		t.Fatal("ensureRouteToCubeDev error=nil, want missing device")
	}
}

func TestGetGatewayMacAddrUsesDefaultRouteNeighbor(t *testing.T) {
	originalLinkByName := netlinkLinkByName
	originalRouteList := netlinkRouteList
	originalNeighList := netlinkNeighList
	defer func() {
		netlinkLinkByName = originalLinkByName
		netlinkRouteList = originalRouteList
		netlinkNeighList = originalNeighList
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp3s0", Index: 2}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		if name != "enp3s0" {
			t.Fatalf("LinkByName(%q), want enp3s0", name)
		}
		return link, nil
	}
	netlinkRouteList = func(gotLink netlink.Link, family int) ([]netlink.Route, error) {
		if gotLink.Attrs().Index != 2 {
			t.Fatalf("RouteList link index=%d, want 2", gotLink.Attrs().Index)
		}
		if family != netlink.FAMILY_V4 {
			t.Fatalf("RouteList family=%d, want FAMILY_V4", family)
		}
		return []netlink.Route{{
			LinkIndex: 2,
			Gw:        net.ParseIP("10.2.0.1"),
			Priority:  100,
		}}, nil
	}
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		if linkIndex != 2 {
			t.Fatalf("NeighList linkIndex=%d, want 2", linkIndex)
		}
		if family != netlink.FAMILY_V4 {
			t.Fatalf("NeighList family=%d, want FAMILY_V4", family)
		}
		return []netlink.Neigh{
			{
				Family:       netlink.FAMILY_V4,
				IP:           net.ParseIP("10.2.127.241"),
				HardwareAddr: mustParseMAC(t, "06:59:30:dd:fe:0b"),
				State:        unix.NUD_REACHABLE,
			},
			{
				Family:       netlink.FAMILY_V4,
				IP:           net.ParseIP("10.2.0.1"),
				HardwareAddr: mustParseMAC(t, "00:d0:4c:10:1f:a5"),
				State:        unix.NUD_STALE,
			},
		}, nil
	}

	got, err := getGatewayMacAddr("enp3s0")
	if err != nil {
		t.Fatalf("getGatewayMacAddr error=%v", err)
	}
	if got != "00:d0:4c:10:1f:a5" {
		t.Fatalf("gateway mac=%s, want 00:d0:4c:10:1f:a5", got)
	}
}

func TestGetGatewayMacAddrRequiresDefaultRoute(t *testing.T) {
	originalLinkByName := netlinkLinkByName
	originalRouteList := netlinkRouteList
	defer func() {
		netlinkLinkByName = originalLinkByName
		netlinkRouteList = originalRouteList
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp3s0", Index: 2}}
	netlinkLinkByName = func(string) (netlink.Link, error) {
		return link, nil
	}
	netlinkRouteList = func(netlink.Link, int) ([]netlink.Route, error) {
		return []netlink.Route{{
			LinkIndex: 2,
			Dst:       mustParseCIDR(t, "10.2.0.0/16"),
		}}, nil
	}

	if _, err := getGatewayMacAddr("enp3s0"); err == nil {
		t.Fatal("getGatewayMacAddr error=nil, want missing default route")
	}
}

func TestGetGatewayMacAddrRequiresGatewayNeighbor(t *testing.T) {
	originalLinkByName := netlinkLinkByName
	originalRouteList := netlinkRouteList
	originalNeighList := netlinkNeighList
	defer func() {
		netlinkLinkByName = originalLinkByName
		netlinkRouteList = originalRouteList
		netlinkNeighList = originalNeighList
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp3s0", Index: 2}}
	netlinkLinkByName = func(string) (netlink.Link, error) {
		return link, nil
	}
	netlinkRouteList = func(netlink.Link, int) ([]netlink.Route, error) {
		return []netlink.Route{{
			LinkIndex: 2,
			Gw:        net.ParseIP("10.2.0.1"),
		}}, nil
	}
	netlinkNeighList = func(int, int) ([]netlink.Neigh, error) {
		return []netlink.Neigh{{
			Family:       netlink.FAMILY_V4,
			IP:           net.ParseIP("10.2.127.241"),
			HardwareAddr: mustParseMAC(t, "06:59:30:dd:fe:0b"),
			State:        unix.NUD_REACHABLE,
		}}, nil
	}

	_, err := getGatewayMacAddr("enp3s0")
	if err == nil {
		t.Fatal("getGatewayMacAddr error=nil, want missing gateway neighbor")
	}
	if !strings.Contains(err.Error(), "via 10.2.0.1") {
		t.Fatalf("getGatewayMacAddr error=%q, want gateway IP", err.Error())
	}
}

func TestGetGatewayMacAddrUsesLowestMetricDefaultRoute(t *testing.T) {
	originalLinkByName := netlinkLinkByName
	originalRouteList := netlinkRouteList
	originalNeighList := netlinkNeighList
	defer func() {
		netlinkLinkByName = originalLinkByName
		netlinkRouteList = originalRouteList
		netlinkNeighList = originalNeighList
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp3s0", Index: 2}}
	netlinkLinkByName = func(string) (netlink.Link, error) {
		return link, nil
	}
	netlinkRouteList = func(netlink.Link, int) ([]netlink.Route, error) {
		return []netlink.Route{
			{
				LinkIndex: 2,
				Gw:        net.ParseIP("10.2.0.254"),
				Priority:  200,
			},
			{
				LinkIndex: 2,
				Gw:        net.ParseIP("10.2.0.1"),
				Priority:  100,
			},
		}, nil
	}
	netlinkNeighList = func(int, int) ([]netlink.Neigh, error) {
		return []netlink.Neigh{
			{
				Family:       netlink.FAMILY_V4,
				IP:           net.ParseIP("10.2.0.254"),
				HardwareAddr: mustParseMAC(t, "06:59:30:dd:fe:0b"),
				State:        unix.NUD_REACHABLE,
			},
			{
				Family:       netlink.FAMILY_V4,
				IP:           net.ParseIP("10.2.0.1"),
				HardwareAddr: mustParseMAC(t, "00:d0:4c:10:1f:a5"),
				State:        unix.NUD_REACHABLE,
			},
		}, nil
	}

	got, err := getGatewayMacAddr("enp3s0")
	if err != nil {
		t.Fatalf("getGatewayMacAddr error=%v", err)
	}
	if got != "00:d0:4c:10:1f:a5" {
		t.Fatalf("gateway mac=%s, want 00:d0:4c:10:1f:a5", got)
	}
}

func TestIsUsableGatewayNeighbor(t *testing.T) {
	gatewayIP := net.ParseIP("10.2.0.1")
	gatewayMAC := mustParseMAC(t, "00:d0:4c:10:1f:a5")
	for _, tc := range []struct {
		name  string
		neigh netlink.Neigh
		want  bool
	}{
		{
			name:  "reachable",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_REACHABLE},
			want:  true,
		},
		{
			name:  "stale",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_STALE},
			want:  true,
		},
		{
			name:  "delay",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_DELAY},
			want:  true,
		},
		{
			name:  "probe",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_PROBE},
			want:  true,
		},
		{
			name:  "permanent",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_PERMANENT},
			want:  true,
		},
		{
			name:  "wrong ip",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: net.ParseIP("10.2.127.241"), HardwareAddr: gatewayMAC, State: unix.NUD_REACHABLE},
		},
		{
			name:  "wrong family",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V6, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_REACHABLE},
		},
		{
			name:  "empty mac",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, State: unix.NUD_REACHABLE},
		},
		{
			name:  "incomplete",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_INCOMPLETE},
		},
		{
			name:  "failed",
			neigh: netlink.Neigh{Family: netlink.FAMILY_V4, IP: gatewayIP, HardwareAddr: gatewayMAC, State: unix.NUD_FAILED},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUsableGatewayNeighbor(tc.neigh, gatewayIP); got != tc.want {
				t.Fatalf("isUsableGatewayNeighbor=%v, want %v", got, tc.want)
			}
		})
	}
}

func mustParseMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", value, err)
	}
	return mac
}

func mustParseCIDR(t *testing.T, value string) *net.IPNet {
	t.Helper()
	_, cidr, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", value, err)
	}
	return cidr
}

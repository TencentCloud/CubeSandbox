// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestWithDumpRetrySucceedsAfterInterrupt(t *testing.T) {
	calls := 0
	got, err := WithDumpRetry(func() (int, error) {
		calls++
		if calls < 3 {
			return 0, netlink.ErrDumpInterrupted
		}
		return 42, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 42, got)
	assert.Equal(t, 3, calls)
}

func TestWithDumpRetryExhaustsAttempts(t *testing.T) {
	calls := 0
	_, err := WithDumpRetry(func() (int, error) {
		calls++
		return 0, netlink.ErrDumpInterrupted
	})
	require.ErrorIs(t, err, netlink.ErrDumpInterrupted)
	assert.Equal(t, maxDumpRetries, calls)
}

func TestWithDumpRetryDoesNotRetryOtherErrors(t *testing.T) {
	want := errors.New("not a dump interrupt")
	calls := 0
	_, err := WithDumpRetry(func() (int, error) {
		calls++
		return 0, want
	})
	require.ErrorIs(t, err, want)
	assert.Equal(t, 1, calls)
}

func TestWithDumpRetryTreatsEINTRAsInterrupt(t *testing.T) {
	calls := 0
	got, err := WithDumpRetry(func() (string, error) {
		calls++
		if calls == 1 {
			return "", unix.EINTR
		}
		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	assert.Equal(t, 2, calls)
}

func TestLookupGatewayMacFindsUsableNeighbor(t *testing.T) {
	gwIP := net.ParseIP("192.168.1.1")
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	origNeighList := netlinkNeighList
	t.Cleanup(func() { netlinkNeighList = origNeighList })

	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return []netlink.Neigh{
			{IP: gwIP, HardwareAddr: mac, Family: netlink.FAMILY_V4, State: unix.NUD_REACHABLE},
		}, nil
	}
	got, err := lookupGatewayMac(1, gwIP)
	require.NoError(t, err)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", got)
}

func TestLookupGatewayMacSkipsIncomplete(t *testing.T) {
	gwIP := net.ParseIP("192.168.1.1")
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	origNeighList := netlinkNeighList
	t.Cleanup(func() { netlinkNeighList = origNeighList })

	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return []netlink.Neigh{
			{IP: gwIP, HardwareAddr: mac, Family: netlink.FAMILY_V4, State: unix.NUD_INCOMPLETE},
		}, nil
	}
	_, err := lookupGatewayMac(1, gwIP)
	require.ErrorIs(t, err, errGatewayMacNotFound)
}

func TestLookupGatewayMacPropagatesNetlinkError(t *testing.T) {
	gwIP := net.ParseIP("192.168.1.1")
	realErr := errors.New("operation not permitted")
	origNeighList := netlinkNeighList
	t.Cleanup(func() { netlinkNeighList = origNeighList })

	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return nil, realErr
	}
	_, err := lookupGatewayMac(1, gwIP)
	require.ErrorIs(t, err, realErr)
	require.False(t, errors.Is(err, errGatewayMacNotFound),
		"real netlink error should not be masked as cache miss")
}

func TestGetGatewayMacAddrReturnsImmediatelyWhenCached(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	mac, _ := net.ParseMAC("de:ad:be:ef:00:01")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
	})

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return []netlink.Neigh{
			{IP: gwIP, HardwareAddr: mac, Family: netlink.FAMILY_V4, State: unix.NUD_REACHABLE},
		}, nil
	}

	arpCalls := 0
	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpCalls++
		return nil
	}

	got, err := GetGatewayMacAddr("ens3")
	require.NoError(t, err)
	assert.Equal(t, "de:ad:be:ef:00:01", got)
	assert.Equal(t, 0, arpCalls, "ARP should not be triggered when cache hits on first try")
}

func TestGetGatewayMacAddrRetriesOnCacheMiss(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	mac, _ := net.ParseMAC("de:ad:be:ef:00:01")

	// Save originals and restore after test.
	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	origBackoff := gatewayARPBackoff
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
		gatewayARPBackoff = origBackoff
	})
	gatewayARPBackoff = 0 // no real sleep in tests

	// Stub link.
	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	// Stub default route.
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}

	// Simulate ARP cache miss for first 2 calls, then succeed.
	neighCalls := 0
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		neighCalls++
		if neighCalls <= 2 {
			return nil, nil // empty neighbor table
		}
		return []netlink.Neigh{
			{IP: gwIP, HardwareAddr: mac, Family: netlink.FAMILY_V4, State: unix.NUD_REACHABLE},
		}, nil
	}

	arpCalls := 0
	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpCalls++
		return nil
	}

	got, err := GetGatewayMacAddr("ens3")
	require.NoError(t, err)
	assert.Equal(t, "de:ad:be:ef:00:01", got)
	assert.Equal(t, 2, arpCalls, "ARP should have been triggered twice")
	assert.Equal(t, 3, neighCalls, "neighbor table should have been queried 3 times")
}

func TestGetGatewayMacAddrFailsAfterAllRetries(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	origBackoff := gatewayARPBackoff
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
		gatewayARPBackoff = origBackoff
	})
	gatewayARPBackoff = 0 // no real sleep in tests

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}
	// Neighbor table is always empty.
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return nil, nil
	}

	arpCalls := 0
	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpCalls++
		return nil
	}

	_, err := GetGatewayMacAddr("ens3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gateway mac for ens3 via 10.0.0.1 not found after 5 retries")
	assert.ErrorIs(t, err, errGatewayMacNotFound)
	assert.Equal(t, gatewayARPRetries, arpCalls, "ARP should be triggered on every retry")
}

func TestGetGatewayMacAddrPropagatesRealErrorsImmediately(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	realErr := errors.New("permission denied")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
	})

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}
	// NeighList returns a real error (not cache miss).
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return nil, realErr
	}

	arpCalls := 0
	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpCalls++
		return nil
	}

	_, err := GetGatewayMacAddr("ens3")
	require.Error(t, err)
	assert.ErrorIs(t, err, realErr)
	assert.Equal(t, 0, arpCalls, "ARP should not be triggered for real netlink errors")
}

func TestProbeNeighborEntryIssuesNeighSet(t *testing.T) {
	gwIP := net.ParseIP("192.168.1.1")

	origNeighSet := netlinkNeighSet
	t.Cleanup(func() { netlinkNeighSet = origNeighSet })

	var captured *netlink.Neigh
	netlinkNeighSet = func(neigh *netlink.Neigh) error {
		captured = neigh
		return nil
	}

	probeNeighborEntry(5, gwIP)
	require.NotNil(t, captured)
	assert.Equal(t, 5, captured.LinkIndex)
	assert.True(t, captured.IP.Equal(gwIP))
	assert.Equal(t, unix.NUD_PROBE, captured.State)
	assert.Equal(t, netlink.FAMILY_V4, captured.Family)
}

func TestProbeNeighborEntryIgnoresError(t *testing.T) {
	gwIP := net.ParseIP("192.168.1.1")

	origNeighSet := netlinkNeighSet
	t.Cleanup(func() { netlinkNeighSet = origNeighSet })

	netlinkNeighSet = func(neigh *netlink.Neigh) error {
		return errors.New("permission denied")
	}

	// Must not panic or return error.
	probeNeighborEntry(1, gwIP)
}

func TestTriggerARPResolutionIntegration(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")

	origNeighSet := netlinkNeighSet
	origUDPProbe := sendUDPProbe
	t.Cleanup(func() {
		netlinkNeighSet = origNeighSet
		sendUDPProbe = origUDPProbe
	})

	neighSetCalls := 0
	netlinkNeighSet = func(neigh *netlink.Neigh) error {
		neighSetCalls++
		return nil
	}

	udpProbeCalls := 0
	sendUDPProbe = func(ip net.IP, localIP net.IP) error {
		udpProbeCalls++
		return nil
	}

	// Exercise the real triggerARPResolution (not mocked).
	probeErr := triggerARPResolution(5, gwIP, nil)

	assert.NoError(t, probeErr)
	assert.Equal(t, 1, neighSetCalls, "NeighSet should be called exactly once")
	assert.Equal(t, 1, udpProbeCalls, "sendUDPProbe should be called exactly once")
}

// TestGetGatewayMacAddrRetriesOnIncompleteNeighbor validates the exact
// scenario from #1608: the neighbor entry exists but is in NUD_INCOMPLETE
// state, and becomes NUD_REACHABLE only after ARP is triggered.
func TestGetGatewayMacAddrRetriesOnIncompleteNeighbor(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	mac, _ := net.ParseMAC("de:ad:be:ef:00:01")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	origBackoff := gatewayARPBackoff
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
		gatewayARPBackoff = origBackoff
	})
	gatewayARPBackoff = 0

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}

	// First read returns NUD_INCOMPLETE (entry exists but not usable);
	// after ARP is triggered, subsequent reads return NUD_REACHABLE.
	arpDone := false
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		if !arpDone {
			return []netlink.Neigh{
				{IP: gwIP, HardwareAddr: nil, Family: netlink.FAMILY_V4, State: unix.NUD_INCOMPLETE},
			}, nil
		}
		return []netlink.Neigh{
			{IP: gwIP, HardwareAddr: mac, Family: netlink.FAMILY_V4, State: unix.NUD_REACHABLE},
		}, nil
	}

	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpDone = true
		return nil
	}

	got, err := GetGatewayMacAddr("ens3")
	require.NoError(t, err)
	assert.Equal(t, "de:ad:be:ef:00:01", got)
}

// TestGetGatewayMacAddrPropagatesMidRetryError validates that a real netlink
// error surfacing mid-retry (after one or more cache-miss lookups) is
// propagated immediately instead of being swallowed by further retries.
func TestGetGatewayMacAddrPropagatesMidRetryError(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	realErr := errors.New("operation not permitted")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	origBackoff := gatewayARPBackoff
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
		gatewayARPBackoff = origBackoff
	})
	gatewayARPBackoff = 0

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}

	// First lookup: empty table (cache miss → enters retry loop).
	// Second lookup: real netlink error → must propagate immediately.
	neighCalls := 0
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		neighCalls++
		if neighCalls == 1 {
			return nil, nil // empty → cache miss
		}
		return nil, realErr
	}

	arpCalls := 0
	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		arpCalls++
		return nil
	}

	_, err := GetGatewayMacAddr("ens3")
	require.Error(t, err)
	assert.ErrorIs(t, err, realErr, "mid-retry real error must propagate immediately")
	assert.Equal(t, 1, arpCalls, "ARP should be triggered exactly once before the error")
	assert.Equal(t, 2, neighCalls, "neighbor table queried twice: initial + one retry")
}

// TestGetGatewayMacAddrReturnsErrorWhenAddrListFails validates that a
// netlinkAddrList failure during the ARP-retry setup is surfaced instead of
// being silently swallowed. Without the bound source address the UDP probe
// would egress on the wrong interface on multi-homed nodes.
func TestGetGatewayMacAddrReturnsErrorWhenAddrListFails(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	addrErr := errors.New("address list failed")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
	})

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	// First neighbor-table read returns cache miss so the code reaches the
	// addr-list step.
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return nil, nil
	}
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return nil, addrErr
	}

	_, err := GetGatewayMacAddr("ens3")
	require.Error(t, err)
	assert.ErrorIs(t, err, addrErr, "addr-list failure must be propagated")
	assert.Contains(t, err.Error(), "list addrs on ens3 for UDP probe")
}

// TestGetGatewayMacAddrIncludesProbeErrorInFinalError validates that when the
// UDP probe fails on every attempt, the final error mentions the probe failure
// so operators can distinguish "probe never fired" from "ARP unanswered".
func TestGetGatewayMacAddrIncludesProbeErrorInFinalError(t *testing.T) {
	gwIP := net.ParseIP("10.0.0.1")
	probeErr := errors.New("no route to host")

	origNeighList := netlinkNeighList
	origLinkByName := netlinkLinkByName
	origRouteList := netlinkRouteList
	origAddrList := netlinkAddrList
	origTriggerARP := triggerARPResolution
	origBackoff := gatewayARPBackoff
	t.Cleanup(func() {
		netlinkNeighList = origNeighList
		netlinkLinkByName = origLinkByName
		netlinkRouteList = origRouteList
		netlinkAddrList = origAddrList
		triggerARPResolution = origTriggerARP
		gatewayARPBackoff = origBackoff
	})
	gatewayARPBackoff = 0

	fakeLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 5, Name: "ens3"}}
	netlinkLinkByName = func(name string) (netlink.Link, error) {
		return fakeLink, nil
	}
	netlinkRouteList = func(link netlink.Link, family int) ([]netlink.Route, error) {
		return []netlink.Route{{Dst: nil, Gw: gwIP, Priority: 100}}, nil
	}
	fakeAddr, _ := netlink.ParseAddr("10.0.0.2/24")
	netlinkAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		return []netlink.Addr{*fakeAddr}, nil
	}
	netlinkNeighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		return nil, nil
	}

	triggerARPResolution = func(linkIndex int, ip net.IP, localIP net.IP) error {
		return probeErr
	}

	_, err := GetGatewayMacAddr("ens3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last probe error", "final error should mention probe failure")
	assert.Contains(t, err.Error(), "no route to host", "final error should include the probe error message")
	assert.ErrorIs(t, err, errGatewayMacNotFound)
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"net"
	"strings"
	"testing"
)

func TestNodeIPFromEnv(t *testing.T) {
	t.Setenv("CUBE_SANDBOX_NODE_IP", " 192.0.2.10 ")

	got, err := nodeIPFromEnv()
	if err != nil {
		t.Fatalf("nodeIPFromEnv() error = %v", err)
	}
	if got != "192.0.2.10" {
		t.Fatalf("nodeIPFromEnv() = %q, want %q", got, "192.0.2.10")
	}
}

func TestNodeIPFromEnvRejectsInvalidAddresses(t *testing.T) {
	for _, value := range []string{"", "not-an-ip", "127.0.0.1", "2001:db8::1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CUBE_SANDBOX_NODE_IP", value)
			if _, err := nodeIPFromEnv(); err == nil {
				t.Fatalf("nodeIPFromEnv() succeeded for %q", value)
			}
		})
	}
}

func TestDefaultRouteInterfaceChoosesLowestMetric(t *testing.T) {
	routes := strings.NewReader(`Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 00000000 0100000A 0003 0 0 100 00000000 0 0 0
tap0 00000000 00000000 0000 0 0 1 00000000 0 0 0
ens5 00000000 0100000A 0003 0 0 10 00000000 0 0 0
eth1 0000000A 00000000 0001 0 0 1 00FFFFFF 0 0 0
`)

	got, err := defaultRouteInterface(routes)
	if err != nil {
		t.Fatalf("defaultRouteInterface() error = %v", err)
	}
	if got != "ens5" {
		t.Fatalf("defaultRouteInterface() = %q, want %q", got, "ens5")
	}
}

func TestDefaultRouteInterfaceWithoutDefaultRoute(t *testing.T) {
	routes := strings.NewReader(`Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 0000000A 00000000 0001 0 0 0 00FFFFFF 0 0 0
`)

	if _, err := defaultRouteInterface(routes); err == nil {
		t.Fatal("defaultRouteInterface() succeeded without a default route")
	}
}

func TestFirstNonLoopbackIPv4(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPAddr{IP: net.ParseIP("192.0.2.20")},
		&net.IPNet{IP: net.ParseIP("198.51.100.30"), Mask: net.CIDRMask(24, 32)},
	}

	if got := firstNonLoopbackIPv4(addrs); got != "192.0.2.20" {
		t.Fatalf("firstNonLoopbackIPv4() = %q, want %q", got, "192.0.2.20")
	}
}

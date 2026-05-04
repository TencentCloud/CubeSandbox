// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"net"
	"testing"
)

func TestNewIPAllocator(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string
		wantErr  bool
		wantGw   string
		wantUsed int // expected usedIPNum after init; 0 means skip check
	}{
		{
			name:    "invalid-cidr",
			cidr:    "not-a-cidr",
			wantErr: true,
		},
		{
			name:    "invalid-ip",
			cidr:    "300.1.0.0/16",
			wantErr: true,
		},
		{
			name:    "ipv6-unsupported",
			cidr:    "2001:db8::/64",
			wantErr: true,
		},
		{
			name:    "mask-too-large",
			cidr:    "192.168.0.0/32",
			wantErr: true,
		},
		{
			name:    "mask-31-too-large",
			cidr:    "192.168.0.0/31",
			wantErr: true,
		},
		{
			name:    "valid-normalization",
			cidr:    "192.168.1.99/24",
			wantErr: false,
			wantGw:  "192.168.1.1",
		},
		{
			name:     "valid-30-reserves-three",
			cidr:     "10.0.0.0/30",
			wantErr:  false,
			wantGw:   "10.0.0.1",
			wantUsed: 3, // network(.0) + gateway(.1) + broadcast(.3)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newIPAllocator(tt.cidr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newIPAllocator() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantGw != "" {
				gw := a.GatewayIP()
				expected := net.ParseIP(tt.wantGw).To4()
				if !gw.Equal(expected) {
					t.Fatalf("GatewayIP() = %v, want %v", gw, expected)
				}
			}
			if tt.wantUsed > 0 && a.usedIPNum != tt.wantUsed {
				t.Fatalf("usedIPNum = %d, want %d", a.usedIPNum, tt.wantUsed)
			}
		})
	}
}

func TestIPAllocatorSafety(t *testing.T) {
	a, err := newIPAllocator("192.168.0.0/24")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		op   string // "release" or "assign"
		ip   string // use "nil" for nil input
	}{
		{"release-out-of-range", "release", "192.168.1.1"},
		{"release-invalid", "release", "not-an-ip"},
		{"release-nil", "release", "nil"},
		{"assign-out-of-range", "assign", "192.168.1.2"},
		{"assign-invalid", "assign", "not-an-ip"},
		{"assign-nil", "assign", "nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ip net.IP
			if tt.ip != "nil" {
				ip = net.ParseIP(tt.ip)
			}

			// Capture usedIPNum and maxIdx to ensure no side effects
			oldUsed := a.usedIPNum
			oldMaxIdx := a.maxIdx

			if tt.op == "release" {
				a.Release(ip)
			} else {
				a.Assign(ip)
			}

			if a.usedIPNum != oldUsed {
				t.Errorf("%s: usedIPNum changed from %d to %d", tt.name, oldUsed, a.usedIPNum)
			}
			if a.maxIdx != oldMaxIdx {
				t.Errorf("%s: maxIdx changed from %d to %d", tt.name, oldMaxIdx, a.maxIdx)
			}
		})
	}
}

func TestIPAllocatorAllocateAndExhaustion(t *testing.T) {
	// small subnet for exhaustion test: /30 gives 4 IPs (size=4)
	// reserved: network (.0), gw (.1), broadcast (.3)
	// usable: 1 IP (.2)
	a, err := newIPAllocator("10.0.0.0/30")
	if err != nil {
		t.Fatal(err)
	}

	// allocate the only available IP
	ip, err := a.Allocate()
	if err != nil {
		t.Fatalf("unexpected error allocating IP: %v", err)
	}
	expected := net.ParseIP("10.0.0.2").To4()
	if !ip.Equal(expected) {
		t.Fatalf("Allocate()=%v, want %v", ip, expected)
	}

	// pool should be exhausted now
	_, err = a.Allocate()
	if err != errIPExhausted {
		t.Fatalf("Allocate() error=%v, want %v", err, errIPExhausted)
	}

	// release the IP and allocate again
	a.Release(expected)
	ip2, err := a.Allocate()
	if err != nil {
		t.Fatalf("unexpected error after release: %v", err)
	}
	if !ip2.Equal(expected) {
		t.Fatalf("Allocate() after release=%v, want %v", ip2, expected)
	}
}


func TestIPAllocatorAssignExist(t *testing.T) {
	a, err := newIPAllocator("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}

	targetIP := net.ParseIP("10.0.0.50").To4()
	
	exists, err := a.exist(targetIP)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("IP should not exist initially")
	}

	a.Assign(targetIP)

	exists, err = a.exist(targetIP)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("IP should exist after Assign")
	}

	a.Release(targetIP)

	exists, err = a.exist(targetIP)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("IP should not exist after Release")
	}
}

func TestExistOutOfRange(t *testing.T) {
	a, err := newIPAllocator("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.exist(net.ParseIP("10.0.1.1").To4())
	if err != errNotInRange {
		t.Fatalf("exist() error = %v, want %v", err, errNotInRange)
	}
}

func TestIPAllocatorIdempotency(t *testing.T) {
	a, err := newIPAllocator("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		setup  func() // optional pre-action
		action func() // the idempotent / noop operation
	}{
		{
			name: "assign-idempotent",
			setup: func() {
				a.Assign(net.ParseIP("10.0.0.50").To4())
			},
			action: func() {
				a.Assign(net.ParseIP("10.0.0.50").To4()) // same IP again
			},
		},
		{
			name: "release-unallocated-noop",
			action: func() {
				a.Release(net.ParseIP("10.0.0.100").To4()) // never allocated
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
			}
			usedBefore := a.usedIPNum
			tt.action()
			if a.usedIPNum != usedBefore {
				t.Fatalf("usedIPNum changed from %d to %d", usedBefore, a.usedIPNum)
			}
		})
	}
}

func TestAssignAdvancesMaxIdx(t *testing.T) {
	a, err := newIPAllocator("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	oldMaxIdx := a.maxIdx

	// Assign an IP with a high index to verify maxIdx advances
	a.Assign(net.ParseIP("10.0.0.200").To4())
	if a.maxIdx <= oldMaxIdx {
		t.Fatalf("Assign high IP did not advance maxIdx: got %d, was %d", a.maxIdx, oldMaxIdx)
	}
}

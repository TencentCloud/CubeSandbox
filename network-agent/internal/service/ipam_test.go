// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"encoding/binary"
	"net"
	"sync"
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
			name:    "mask-too-small",
			cidr:    "10.0.0.0/7",
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

func TestNewIPAllocatorStartIdxNormalization(t *testing.T) {
	// netip.ParsePrefix does NOT auto-mask the host bits (unlike net.ParseCIDR).
	// Verify that prefix.Masked() in newIPAllocator correctly normalizes
	// "192.168.1.99/24" to a startIdx corresponding to "192.168.1.0".
	tests := []struct {
		name         string
		cidr         string
		wantStartIdx int
	}{
		{
			name:         "host-bits-masked-class-c",
			cidr:         "192.168.1.99/24",
			wantStartIdx: int(binary.BigEndian.Uint32(net.ParseIP("192.168.1.0").To4())),
		},
		{
			name:         "already-canonical",
			cidr:         "10.0.0.0/24",
			wantStartIdx: int(binary.BigEndian.Uint32(net.ParseIP("10.0.0.0").To4())),
		},
		{
			name:         "host-bits-masked-slash-20",
			cidr:         "172.16.5.130/20",
			wantStartIdx: int(binary.BigEndian.Uint32(net.ParseIP("172.16.0.0").To4())),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newIPAllocator(tt.cidr)
			if err != nil {
				t.Fatal(err)
			}
			if a.startIdx != tt.wantStartIdx {
				t.Fatalf("startIdx = %d (0x%08x), want %d (0x%08x)",
					a.startIdx, a.startIdx, tt.wantStartIdx, tt.wantStartIdx)
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

func TestIPAllocatorAssign(t *testing.T) {
	a, _ := newIPAllocator("10.0.0.0/24")

	tests := []struct {
		name       string
		ip         net.IP
		wantMaxIdx int
		wantUsed   int // delta used
	}{
		{"assign-normal", net.ParseIP("10.0.0.50").To4(), 50, 1},
		{"assign-duplicate", net.ParseIP("10.0.0.50").To4(), 50, 0},
		{"assign-high", net.ParseIP("10.0.0.200").To4(), 200, 1},
		{"assign-out-of-range", net.ParseIP("10.0.1.1").To4(), 200, 0},
		{"assign-nil", nil, 200, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldUsed := a.usedIPNum
			a.Assign(tt.ip)
			if a.maxIdx != tt.wantMaxIdx {
				t.Errorf("maxIdx = %v, want %v", a.maxIdx, tt.wantMaxIdx)
			}
			if a.usedIPNum-oldUsed != tt.wantUsed {
				t.Errorf("usedIPNum delta = %v, want %v", a.usedIPNum-oldUsed, tt.wantUsed)
			}
		})
	}
}

func TestIPAllocatorAllocateAfterReleaseWithHighMaxIdx(t *testing.T) {
	// Use a /24 subnet (size 256).
	a, _ := newIPAllocator("10.0.0.0/24")

	// Assign a high IP (10.0.0.200) to advance maxIdx to 200.
	highIP := net.ParseIP("10.0.0.200").To4()
	a.Assign(highIP)
	if a.maxIdx != 200 {
		t.Fatalf("maxIdx should be 200, got %d", a.maxIdx)
	}

	// Release it.
	a.Release(highIP)

	// The next Allocate should start from (maxIdx + 1) % size.
	// 201 is available, so it should return 10.0.0.201.
	ip, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	expected := net.ParseIP("10.0.0.201").To4()
	if !ip.Equal(expected) {
		t.Fatalf("Allocate() = %v, want %v", ip, expected)
	}

	// Next Allocate should return 10.0.0.202.
	ip2, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	expected2 := net.ParseIP("10.0.0.202").To4()
	if !ip2.Equal(expected2) {
		t.Fatalf("Allocate() = %v, want %v", ip2, expected2)
	}
}

func TestIPAllocatorRelease(t *testing.T) {
	a, _ := newIPAllocator("10.0.0.0/24")
	ip := net.ParseIP("10.0.0.50").To4()
	a.Assign(ip)

	tests := []struct {
		name     string
		ip       net.IP
		wantUsed int // delta used
	}{
		{"release-normal", ip, -1},
		{"release-duplicate", ip, 0},
		{"release-unassigned", net.ParseIP("10.0.0.100").To4(), 0},
		{"release-out-of-range", net.ParseIP("10.0.1.1").To4(), 0},
		{"release-nil", nil, 0},
		{"release-network", net.ParseIP("10.0.0.0").To4(), 0},
		{"release-gateway", net.ParseIP("10.0.0.1").To4(), 0},
		{"release-broadcast", net.ParseIP("10.0.0.255").To4(), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldUsed := a.usedIPNum
			a.Release(tt.ip)
			if a.usedIPNum-oldUsed != tt.wantUsed {
				t.Errorf("usedIPNum delta = %v, want %v", a.usedIPNum-oldUsed, tt.wantUsed)
			}
		})
	}
}

func TestIPAllocatorConcurrency(t *testing.T) {
	a, err := newIPAllocator("10.0.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	const (
		numGoroutines = 10
		opsPerG       = 100
	)
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Use sync.Map to track allocated IPs and detect duplicates
	allocatedIPs := sync.Map{}
	baseUsed := a.usedIPNum

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				ip, err := a.Allocate()
				if err != nil {
					t.Errorf("Allocate failed: %v", err)
					return
				}
				ipStr := ip.String()
				if _, loaded := allocatedIPs.LoadOrStore(ipStr, true); loaded {
					t.Errorf("Duplicate IP allocated: %s", ipStr)
				}
			}
		}()
	}
	wg.Wait()

	expectedUsed := baseUsed + (numGoroutines * opsPerG)
	if a.usedIPNum != expectedUsed {
		t.Errorf("usedIPNum mismatch: got %d, want %d", a.usedIPNum, expectedUsed)
	}

	// Re-collect all IPs into a slice for concurrent release
	var ips []string
	allocatedIPs.Range(func(key, value interface{}) bool {
		ips = append(ips, key.(string))
		return true
	})

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		start := i * opsPerG
		end := (i + 1) * opsPerG
		go func(subset []string) {
			defer wg.Done()
			for _, ipStr := range subset {
				a.Release(net.ParseIP(ipStr))
			}
		}(ips[start:end])
	}
	wg.Wait()

	if a.usedIPNum != baseUsed {
		t.Errorf("usedIPNum should return to %d after release, got %d", baseUsed, a.usedIPNum)
	}
}

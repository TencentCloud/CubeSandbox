// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
)

var (
	// errIPExhausted is returned when every usable address in the sandbox CIDR is
	// either reserved or currently assigned.
	errIPExhausted = errors.New("ip exhausted")
)

const (
	// The allocator intentionally accepts only medium-sized IPv4 CIDRs: smaller
	// ranges are too easy to exhaust, while larger ranges would create oversized
	// bitmaps and very long scans on busy nodes.
	sandboxCIDRMinMask = 16
	sandboxCIDRMaxMask = 24
)

// ipAllocator is a bitmap-backed allocator for sandbox IPv4 addresses. Index 0
// is the network address, index 1 is the cube-dev gateway, and the last index is
// broadcast; all three are permanently reserved.
type ipAllocator struct {
	sync.Mutex
	maxIdx    int
	mask      int
	gwIP      net.IP
	size      int
	startIdx  int
	usedIPNum int
	bitmap    []byte
	reserved  map[int]struct{}
}

// newIPAllocator validates the sandbox CIDR and initializes the reservation
// bitmap. Allocation starts after the gateway so sandbox addresses never collide
// with cube-dev.
func newIPAllocator(cidr string) (*ipAllocator, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	if !prefix.Addr().Is4() {
		return nil, &net.ParseError{Type: "cidr address", Text: cidr}
	}
	mask := prefix.Bits()
	if mask < sandboxCIDRMinMask || mask > sandboxCIDRMaxMask {
		return nil, &net.ParseError{Type: "cidr mask fail", Text: cidr}
	}
	size := 1 << (32 - mask)
	byteNum := (size + 7) / 8

	netAddr := prefix.Masked().Addr()
	b := netAddr.As4()
	startIdx := int(binary.BigEndian.Uint32(b[:]))

	allocator := &ipAllocator{
		maxIdx:    1, // Start allocation from idx 2 (after GW)
		mask:      mask,
		size:      size,
		startIdx:  startIdx,
		bitmap:    make([]byte, byteNum),
		reserved:  make(map[int]struct{}),
		usedIPNum: 0,
	}

	// Reserve the network address (idx 0), gateway (idx 1), and broadcast (last idx).
	allocator.reserveIdx(0)
	allocator.reserveIdx(1)
	allocator.reserveIdx(size - 1)
	allocator.gwIP = allocator.idx2IP(1)
	return allocator, nil
}

// GatewayIP returns the cube-dev gateway address reserved at index 1.
func (a *ipAllocator) GatewayIP() net.IP {
	return a.gwIP
}

func (a *ipAllocator) existIdx(idx int) bool {
	return a.bitmap[idx/8]&(1<<(idx%8)) != 0
}

func (a *ipAllocator) setUsed(idx int) {
	a.usedIPNum++
	a.bitmap[idx/8] |= 1 << (idx % 8)
}

func (a *ipAllocator) setUnused(idx int) {
	a.usedIPNum--
	a.bitmap[idx/8] &^= 1 << (idx % 8)
}

func (a *ipAllocator) reserveIdx(idx int) {
	if !a.existIdx(idx) {
		a.setUsed(idx)
	}
	a.reserved[idx] = struct{}{}
}

// ReserveLastUsable permanently withholds the last usable addresses. The
// cube-router fallback mode consumes addresses from the tail of the sandbox CIDR
// for the router local IP and NAT IP.
func (a *ipAllocator) ReserveLastUsable(count int) {
	a.Lock()
	defer a.Unlock()
	if count <= 0 {
		return
	}
	first := a.size - 1 - count
	if first < 2 {
		first = 2
	}
	for idx := first; idx < a.size-1; idx++ {
		a.reserveIdx(idx)
	}
}

func (a *ipAllocator) ip2Idx(ipv4 net.IP) int {
	return int(binary.BigEndian.Uint32(ipv4))
}

// idx2IP computes the net.IP from an offset index.
func (a *ipAllocator) idx2IP(idx int) net.IP {
	ipInt := uint32(a.startIdx + idx)
	return net.IPv4(byte(ipInt>>24), byte(ipInt>>16), byte(ipInt>>8), byte(ipInt)).To4()
}

// Allocate returns the next free sandbox IPv4 address using a rotating cursor so
// repeated create/release cycles do not always reuse the same low address.
func (a *ipAllocator) Allocate() (net.IP, error) {
	a.Lock()
	defer a.Unlock()
	if a.usedIPNum >= a.size {
		return nil, errIPExhausted
	}
	for range a.size {
		a.maxIdx = (a.maxIdx + 1) % a.size
		idx := a.maxIdx
		if !a.existIdx(idx) {
			a.setUsed(idx)
			return a.idx2IP(idx), nil
		}
	}
	return nil, errIPExhausted
}

// Release makes an address available again unless it is outside the managed
// CIDR or belongs to the permanent reservation set.
func (a *ipAllocator) Release(ip net.IP) {
	a.Lock()
	defer a.Unlock()
	ipv4 := ip.To4()
	if ipv4 == nil {
		return
	}
	idx := a.ip2Idx(ipv4) - a.startIdx
	if idx < 0 || idx >= a.size {
		return
	}
	if _, ok := a.reserved[idx]; ok {
		return
	}
	if a.existIdx(idx) {
		a.setUnused(idx)
	}
}

// Assign marks an address as already in use during startup recovery. This lets
// the allocator rebuild its bitmap from live state files and kernel inventory.
func (a *ipAllocator) Assign(ip net.IP) {
	a.Lock()
	defer a.Unlock()
	ipv4 := ip.To4()
	if ipv4 == nil {
		return
	}
	idx := a.ip2Idx(ipv4) - a.startIdx
	if idx < 0 || idx >= a.size {
		return
	}
	if !a.existIdx(idx) {
		a.setUsed(idx)
	}
	if idx > a.maxIdx {
		a.maxIdx = idx
	}
}

const (
	// CubeVS host-port mappings own 20000-29999. SNAT starts immediately above
	// this range so MASQUERADE source ports cannot collide with explicit mappings.
	portMin    uint16 = 20000
	portMax    uint16 = 29999
	tcpPortMax uint16 = 65535
)

// cubeSNATPortRange returns the TCP source-port range reserved for cube-router
// MASQUERADE rules.
func cubeSNATPortRange() (uint16, uint16) {
	return portMax + 1, tcpPortMax
}

func cubeSNATPortMin() uint16 {
	min, _ := cubeSNATPortRange()
	return min
}

const systemPortOwner = "__system__"

// PortBinder owns host-port allocation for CubeVS mappings. Allocate (Reserve)
// marks ports owned immediately; ReleaseOwnership frees them. Create-path
// rollback and cleanup both call ReleaseOwnership, and CubeVS deletes are
// idempotent for mappings that were never installed.
type PortBinder struct {
	mu       sync.Mutex
	min      uint16
	max      uint16
	next     uint16
	assigned map[uint16]string
	owners   map[string]map[uint16]struct{}
}

// newPortBinder seeds the managed range with kernel-reserved ports so CubeVS
// never allocates a port the node administrator has explicitly excluded.
func newPortBinder() (*PortBinder, error) {
	binder := &PortBinder{
		min:      portMin,
		max:      portMax,
		next:     portMin,
		assigned: make(map[uint16]string),
		owners:   make(map[string]map[uint16]struct{}),
	}
	reservedPorts, err := getReservedPorts()
	if err != nil {
		return nil, err
	}
	for _, port := range reservedPorts {
		if port < portMin || port > portMax {
			continue
		}
		binder.assigned[port] = systemPortOwner
	}
	return binder, nil
}

// Reserve allocates concrete host ports for a sandbox and marks them owned.
// Any error rolls back the ports allocated during this call.
func (b *PortBinder) Reserve(owner string, requestedMappings []PortMapping, defaultHostIP string) ([]PortMapping, error) {
	if owner == "" {
		return nil, fmt.Errorf("port binding owner is empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	actualMappings := make([]PortMapping, 0, len(requestedMappings))
	ownedNow := make([]uint16, 0, len(requestedMappings))
	seenHostPorts := make(map[uint16]struct{}, len(requestedMappings))
	for _, mapping := range requestedMappings {
		if err := validatePortMappingPorts(mapping); err != nil {
			b.releaseOwnedLocked(owner, ownedNow)
			return nil, err
		}
		hostPort := mapping.HostPort
		automatic := hostPort == 0
		var port uint16
		if automatic {
			allocatedPort, err := b.allocateLocked(owner)
			if err != nil {
				b.releaseOwnedLocked(owner, ownedNow)
				return nil, err
			}
			port = allocatedPort
			hostPort = int32(port)
		} else {
			port = uint16(hostPort)
		}
		if _, duplicate := seenHostPorts[port]; duplicate {
			if automatic {
				// allocateLocked already inserted this ownership.
				ownedNow = append(ownedNow, port)
			}
			b.releaseOwnedLocked(owner, ownedNow)
			return nil, fmt.Errorf("host port %d appears more than once in one reservation request", port)
		}
		preOwned := false
		if !automatic {
			preOwned = b.assigned[port] == owner
			if err := b.ownLocked(owner, port); err != nil {
				b.releaseOwnedLocked(owner, ownedNow)
				return nil, err
			}
		}
		seenHostPorts[port] = struct{}{}
		if !preOwned {
			ownedNow = append(ownedNow, port)
		}
		actualMappings = append(actualMappings, PortMapping{
			Protocol:      nonEmpty(mapping.Protocol, "tcp"),
			HostIP:        nonEmpty(mapping.HostIP, defaultHostIP),
			HostPort:      int32(hostPort),
			ContainerPort: mapping.ContainerPort,
		})
	}
	return actualMappings, nil
}

// ReleaseOwnership releases all ports owned by a sandbox after its runtime
// mappings have been cleaned (or when a create path rolls back before install).
func (b *PortBinder) ReleaseOwnership(owner string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for port := range b.owners[owner] {
		if b.assigned[port] == owner {
			delete(b.assigned, port)
		}
	}
	delete(b.owners, owner)
}

// Assign marks a recovered live host port as unavailable without assigning it
// to a sandbox owner. Used when reconstructing state from CubeVS maps.
func (b *PortBinder) Assign(port uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.assigned[port]; !ok {
		b.assigned[port] = systemPortOwner
	}
}

// AssignOwner rebuilds ownership from recovered state files. It fails if another
// sandbox already owns the same port.
func (b *PortBinder) AssignOwner(owner string, mappings []PortMapping) error {
	if owner == "" {
		return fmt.Errorf("port binding owner is empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ports := make(map[uint16]struct{}, len(mappings))
	for _, mapping := range mappings {
		if err := validatePortMappingPorts(mapping); err != nil {
			return err
		}
		if mapping.HostPort == 0 {
			continue
		}
		port := uint16(mapping.HostPort)
		if _, duplicate := ports[port]; duplicate {
			return fmt.Errorf("host port %d appears more than once for owner %s", port, owner)
		}
		assignedOwner, taken := b.assigned[port]
		if taken && assignedOwner != owner && assignedOwner != systemPortOwner {
			return fmt.Errorf("host port %d is assigned to %s, want %s", port, assignedOwner, owner)
		}
		ports[port] = struct{}{}
	}

	if len(ports) == 0 {
		return nil
	}
	if b.owners[owner] == nil {
		b.owners[owner] = make(map[uint16]struct{})
	}
	for port := range ports {
		b.assigned[port] = owner
		b.owners[owner][port] = struct{}{}
	}
	return nil
}

func validatePortMappingPorts(mapping PortMapping) error {
	if mapping.ContainerPort < 1 || mapping.ContainerPort > int32(tcpPortMax) {
		return fmt.Errorf("container port %d outside valid range 1-%d", mapping.ContainerPort, tcpPortMax)
	}
	if mapping.HostPort < 0 || mapping.HostPort > int32(tcpPortMax) {
		return fmt.Errorf("host port %d outside valid range 0-%d", mapping.HostPort, tcpPortMax)
	}
	return nil
}

// allocateLocked picks the next free port in the managed range. b.mu must be held.
func (b *PortBinder) allocateLocked(owner string) (uint16, error) {
	span := int(b.max-b.min) + 1
	for i := 0; i < span; i++ {
		p := b.next
		if b.next == b.max {
			b.next = b.min
		} else {
			b.next++
		}
		if _, ok := b.assigned[p]; ok {
			continue
		}
		if err := b.ownLocked(owner, p); err != nil {
			return 0, err
		}
		return p, nil
	}
	return 0, fmt.Errorf("host port exhausted")
}

// ownLocked records one host port as owned by owner. b.mu must be held.
func (b *PortBinder) ownLocked(owner string, port uint16) error {
	if port < b.min || port > b.max {
		return fmt.Errorf("host port %d outside managed range %d-%d", port, b.min, b.max)
	}
	assignedOwner, taken := b.assigned[port]
	if taken && assignedOwner != owner {
		return fmt.Errorf("host port %d is already assigned", port)
	}
	b.assigned[port] = owner
	if b.owners[owner] == nil {
		b.owners[owner] = make(map[uint16]struct{})
	}
	b.owners[owner][port] = struct{}{}
	return nil
}

// releaseOwnedLocked removes the given owned ports for owner. b.mu must be held.
func (b *PortBinder) releaseOwnedLocked(owner string, ports []uint16) {
	for _, port := range ports {
		if b.assigned[port] == owner {
			delete(b.assigned, port)
		}
		if b.owners[owner] != nil {
			delete(b.owners[owner], port)
		}
	}
	if len(b.owners[owner]) == 0 {
		delete(b.owners, owner)
	}
}

// getReservedPorts parses /proc/sys/net/ipv4/ip_local_reserved_ports, including
// comma-separated ranges, into individual port numbers.
func getReservedPorts() ([]uint16, error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_reserved_ports")
	if err != nil {
		return nil, fmt.Errorf("read reserved ports failed: %w", err)
	}
	reservedPortsStr := strings.TrimSpace(string(data))
	if reservedPortsStr == "" {
		return []uint16{}, nil
	}
	ports := strings.Split(reservedPortsStr, ",")
	var reservedPorts []uint16
	for _, port := range ports {
		port = strings.TrimSpace(port)
		if port == "" {
			continue
		}
		if strings.Contains(port, "-") {
			portRange := strings.Split(port, "-")
			if len(portRange) != 2 {
				return nil, fmt.Errorf("invalid reserved port range: %s", port)
			}
			lowerPort, err := strconv.Atoi(portRange[0])
			if err != nil {
				return nil, fmt.Errorf("invalid reserved port range: %s", port)
			}
			upperPort, err := strconv.Atoi(portRange[1])
			if err != nil {
				return nil, fmt.Errorf("invalid reserved port range: %s", port)
			}
			for i := lowerPort; i <= upperPort; i++ {
				reservedPorts = append(reservedPorts, uint16(i))
			}
			continue
		}
		portInt, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("invalid reserved port: %s", port)
		}
		reservedPorts = append(reservedPorts, uint16(portInt))
	}
	return reservedPorts, nil
}

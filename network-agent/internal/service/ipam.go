// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
)

var (
	errIPExhausted = errors.New("ip exhausted")
)

type ipAllocator struct {
	sync.Mutex
	maxIdx    int
	mask      int
	gwIP      net.IP
	size      int
	startIdx  int
	usedIPNum int
	bitmap    []byte
}

func newIPAllocator(cidr string) (*ipAllocator, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	if !prefix.Addr().Is4() {
		return nil, &net.ParseError{Type: "cidr address", Text: cidr}
	}
	mask := prefix.Bits()
	if mask < 8 || mask > 30 {
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
		usedIPNum: 0,
	}

	// Reserve the network address (idx 0), gateway (idx 1), and broadcast (last idx).
	allocator.setUsed(0)
	allocator.setUsed(1)
	allocator.setUsed(size - 1)
	allocator.gwIP = allocator.idx2IP(1)
	return allocator, nil
}

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

func (a *ipAllocator) ip2Idx(ipv4 net.IP) int {
	return int(binary.BigEndian.Uint32(ipv4))
}

// idx2IP computes the net.IP from an offset index.
func (a *ipAllocator) idx2IP(idx int) net.IP {
	ipInt := uint32(a.startIdx + idx)
	return net.IPv4(byte(ipInt>>24), byte(ipInt>>16), byte(ipInt>>8), byte(ipInt)).To4()
}

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
	if idx <= 1 || idx == a.size-1 {
		return
	}
	if a.existIdx(idx) {
		a.setUnused(idx)
	}
}

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

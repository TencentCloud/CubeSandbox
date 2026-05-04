// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
)

var errNotInRange = errors.New("ip not in allocator range")

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
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	netIP := ipNet.IP.To4()
	mask, bits := ipNet.Mask.Size()
	if netIP == nil || bits != 32 {
		return nil, &net.ParseError{Type: "cidr address", Text: cidr}
	}
	if mask > 30 {
		return nil, &net.ParseError{Type: "cidr mask fail", Text: cidr}
	}
	size := 1 << (32 - mask)
	byteNum := size / 8
	if size%8 != 0 {
		byteNum++
	}
	allocator := &ipAllocator{
		mask:      mask,
		size:      size,
		maxIdx:    1,
		bitmap:    make([]byte, byteNum),
		usedIPNum: 0,
	}
	allocator.startIdx = allocator.ip2Idx(netIP)

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

func (a *ipAllocator) exist(ip net.IP) (bool, error) {
	idx := a.ip2Idx(ip) - a.startIdx
	if idx < 0 || idx >= a.size {
		return false, errNotInRange
	}
	return a.existIdx(idx), nil
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

func (a *ipAllocator) ip2Idx(ip net.IP) int {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return -1
	}
	return int(binary.BigEndian.Uint32(ipv4))
}

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
	if ip == nil || ip.To4() == nil {
		return
	}
	idx := a.ip2Idx(ip) - a.startIdx
	if idx < 0 || idx >= a.size {
		return
	}
	if a.existIdx(idx) {
		a.setUnused(idx)
	}
}

func (a *ipAllocator) Assign(ip net.IP) {
	a.Lock()
	defer a.Unlock()
	if ip == nil || ip.To4() == nil {
		return
	}
	idx := a.ip2Idx(ip) - a.startIdx
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

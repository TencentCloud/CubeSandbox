// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	portMin    uint16 = 20000
	portMax    uint16 = 29999
	tcpPortMax uint16 = 65535
)

func cubeSNATPortRange() (uint16, uint16) {
	return portMax + 1, tcpPortMax
}

type portAllocator struct {
	mu       sync.Mutex
	min      uint16
	max      uint16
	next     uint16
	assigned map[uint16]struct{}
}

func newPortAllocator() (*portAllocator, error) {
	alloc := &portAllocator{
		min:      portMin,
		max:      portMax,
		next:     portMin,
		assigned: make(map[uint16]struct{}),
	}
	reservedPorts, err := getReservedPorts()
	if err != nil {
		return nil, err
	}
	for _, port := range reservedPorts {
		if port < portMin || port > portMax {
			continue
		}
		alloc.assigned[port] = struct{}{}
	}
	return alloc, nil
}

func (a *portAllocator) Allocate() (uint16, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	span := int(a.max-a.min) + 1
	for i := 0; i < span; i++ {
		p := a.next
		if a.next == a.max {
			a.next = a.min
		} else {
			a.next++
		}
		if _, ok := a.assigned[p]; ok {
			continue
		}
		a.assigned[p] = struct{}{}
		return p, nil
	}
	return 0, fmt.Errorf("host port exhausted")
}

func (a *portAllocator) Release(port uint16) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.assigned, port)
}

func (a *portAllocator) Assign(port uint16) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.assigned[port] = struct{}{}
}

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

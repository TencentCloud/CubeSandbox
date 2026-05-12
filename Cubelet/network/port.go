// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/sysctl"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var DefaultExposedPorts = []uint16{49983}

func initPortAllocatorFromSysConfig() (allocator.Allocator[uint16], error) {
	_, upperPort, err := getLocalPortRange()
	if err != nil {
		return nil, err
	}

	upperPort = upperPort + 1
	CubeLog.Errorf("port allocator in range [%d, 65535]", upperPort)
	portRanger, err := allocator.NewSimpleLinearRanger(upperPort, 65535)
	if err != nil {
		return nil, err
	}
	alloc := allocator.NewAllocator[uint16](portRanger)

	reservedPorts, err := getAndParseReservedPorts()
	if err != nil {
		return nil, err
	}

	CubeLog.Errorf("try to reserve ports %v", reservedPorts)
	for _, port := range reservedPorts {
		if !portRanger.Contains(uint16(port)) {
			continue
		}
		if err := alloc.Assign(uint16(port)); err != nil {
			return nil, fmt.Errorf("failed to reserve port %d: %w", port, err)
		}
	}

	return alloc, nil
}

func getLocalPortRange() (uint16, uint16, error) {
	portRange, err := sysctl.Get("net.ipv4.ip_local_port_range")
	if err != nil {
		return 0, 0, fmt.Errorf("get port range from sysctl failed: %w", err)
	}
	portRange = strings.TrimSpace(portRange)

	ports := strings.Fields(portRange)
	if len(ports) != 2 {
		return 0, 0, fmt.Errorf("invalid port range: %s", portRange)
	}
	lowerPort, err := strconv.Atoi(ports[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range: %s", portRange)
	}
	upperPort, err := strconv.Atoi(ports[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid port range: %s", portRange)
	}
	return uint16(lowerPort), uint16(upperPort), nil
}

func getAndParseReservedPorts() ([]uint16, error) {

	reservedPortsStr, err := sysctl.Get("net.ipv4.ip_local_reserved_ports")
	if err != nil {
		return nil, fmt.Errorf("get reserved ports from sysctl failed: %w", err)
	}
	reservedPortsStr = strings.TrimSpace(reservedPortsStr)
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
				return nil, fmt.Errorf("invalid port range: %s", port)
			}
			lowerPort, err := strconv.Atoi(portRange[0])
			if err != nil {
				return nil, fmt.Errorf("invalid port range: %s", port)
			}
			upperPort, err := strconv.Atoi(portRange[1])
			if err != nil {
				return nil, fmt.Errorf("invalid port range: %s", port)
			}
			for i := lowerPort; i <= upperPort; i++ {
				reservedPorts = append(reservedPorts, uint16(i))
			}
		} else {

			portInt, err := strconv.Atoi(port)
			if err != nil {
				return nil, fmt.Errorf("invalid port range: %s", port)
			}
			reservedPorts = append(reservedPorts, uint16(portInt))
		}
	}
	return reservedPorts, nil
}

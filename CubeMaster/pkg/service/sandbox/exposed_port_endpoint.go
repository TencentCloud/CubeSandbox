// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

const (
	ExposedPortModeTapIP    = "tap_ip"
	ExposedPortModeHostPort = "host_port"
)

type ExposedPortEndpoint struct {
	Address       string
	Mode          string
	HostIP        string
	SandboxIP     string
	ContainerPort int32
	HostPort      int32
}

func ResolveExposedPortEndpoint(callerHostIP string, proxyMap *proxytypes.SandboxProxyMap, containerPort int32) (*ExposedPortEndpoint, error) {
	if proxyMap == nil {
		return nil, fmt.Errorf("sandbox proxy metadata is missing")
	}
	if containerPort <= 0 {
		return nil, fmt.Errorf("container port %d is invalid", containerPort)
	}

	containerPortStr := strconv.Itoa(int(containerPort))
	if callerHostIP != "" && callerHostIP == proxyMap.HostIP {
		sandboxIP := strings.TrimSpace(proxyMap.SandboxIP)
		if sandboxIP == "" {
			return nil, fmt.Errorf("sandbox ip is empty for local endpoint resolution")
		}
		return &ExposedPortEndpoint{
			Address:       net.JoinHostPort(sandboxIP, containerPortStr),
			Mode:          ExposedPortModeTapIP,
			HostIP:        proxyMap.HostIP,
			SandboxIP:     sandboxIP,
			ContainerPort: containerPort,
		}, nil
	}

	if proxyMap.HostIP == "" {
		return nil, fmt.Errorf("host ip is empty for host-port endpoint resolution")
	}
	hostPortStr, ok := proxyMap.ContainerToHostPorts[containerPortStr]
	if !ok || strings.TrimSpace(hostPortStr) == "" {
		return nil, fmt.Errorf("host port mapping for container port %s is missing", containerPortStr)
	}
	hostPort, err := strconv.Atoi(hostPortStr)
	if err != nil {
		return nil, fmt.Errorf("host port mapping for container port %s is invalid: %w", containerPortStr, err)
	}
	return &ExposedPortEndpoint{
		Address:       net.JoinHostPort(proxyMap.HostIP, hostPortStr),
		Mode:          ExposedPortModeHostPort,
		HostIP:        proxyMap.HostIP,
		SandboxIP:     proxyMap.SandboxIP,
		ContainerPort: containerPort,
		HostPort:      int32(hostPort),
	}, nil
}

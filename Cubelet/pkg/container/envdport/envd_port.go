// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package envdport assigns the envd listener used to address each container
// inside a sandbox network namespace.
package envdport

import (
	"fmt"
	"strconv"
	"strings"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

const (
	// Default is the established envd port used by the primary container.
	Default = 49983
	// EnvironmentVariable is read by envd when it starts in a container.
	EnvironmentVariable = "ENVD_PORT"
)

// Assign gives every container a distinct envd listener. Existing valid
// ENVD_PORT values are preserved; missing sidecar values are allocated
// sequentially after the primary container's established port.
func Assign(containers []*cubebox.ContainerConfig) error {
	used := make(map[int]string, len(containers))

	// Reserve explicit values first so an earlier implicit container cannot
	// accidentally claim a later container's configured port.
	for index, container := range containers {
		port, explicit, err := explicit(container)
		if err != nil {
			return err
		}
		if !explicit {
			continue
		}
		if index == 0 && port != Default {
			return fmt.Errorf("primary container %q must use ENVD_PORT %d", container.GetName(), Default)
		}
		if index > 0 && port == Default {
			return fmt.Errorf("ENVD_PORT %d is reserved for the primary container", Default)
		}
		if owner, exists := used[port]; exists {
			return fmt.Errorf("duplicate ENVD_PORT %d for containers %q and %q", port, owner, container.GetName())
		}
		used[port] = container.GetName()
	}
	if len(containers) > 0 {
		used[Default] = containers[0].GetName()
	}

	next := Default + 1
	for index, container := range containers {
		port, configured, _ := explicit(container)
		if configured {
			continue
		}
		if index == 0 {
			port = Default
		} else {
			for {
				if next > 65535 {
					return fmt.Errorf("no available TCP port for container %q envd endpoint", container.GetName())
				}
				if _, exists := used[next]; !exists {
					port = next
					used[port] = container.GetName()
					next++
					break
				}
				next++
			}
		}
		container.Envs = append(container.Envs, &cubebox.KeyValue{
			Key:   EnvironmentVariable,
			Value: strconv.Itoa(port),
		})
	}
	return nil
}

// PrepareRequest assigns per-container envd ports and ensures the sandbox
// network exposes each one through CubeProxy. It must run before network
// allocation so remote proxy nodes receive host-port mappings for sidecars.
func PrepareRequest(req *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return fmt.Errorf("sandbox request is nil")
	}
	if err := Assign(req.GetContainers()); err != nil {
		return err
	}

	exposed := make(map[int64]struct{}, len(req.GetExposedPorts())+len(req.GetContainers()))
	for _, port := range req.GetExposedPorts() {
		exposed[port] = struct{}{}
	}
	for _, container := range req.GetContainers() {
		port, ok := Get(container)
		if !ok {
			return fmt.Errorf("container %q has no assigned envd port", container.GetName())
		}
		value := int64(port)
		if _, exists := exposed[value]; exists {
			continue
		}
		req.ExposedPorts = append(req.ExposedPorts, value)
		exposed[value] = struct{}{}
	}
	return nil
}

// Get returns the configured envd port after Assign has run.
func Get(container *cubebox.ContainerConfig) (int, bool) {
	port, configured, err := explicit(container)
	return port, configured && err == nil
}

func explicit(container *cubebox.ContainerConfig) (int, bool, error) {
	if container == nil {
		return 0, false, fmt.Errorf("container configuration is nil")
	}
	for _, env := range container.GetEnvs() {
		if env == nil || env.GetKey() != EnvironmentVariable {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(env.GetValue()))
		if err != nil || port < 1 || port > 65535 {
			return 0, false, fmt.Errorf("container %q has invalid ENVD_PORT %q", container.GetName(), env.GetValue())
		}
		return port, true, nil
	}
	return 0, false, nil
}

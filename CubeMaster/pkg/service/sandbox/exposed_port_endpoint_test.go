// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

func TestResolveExposedPortEndpoint(t *testing.T) {
	t.Run("same host uses tap ip", func(t *testing.T) {
		proxyMap := &proxytypes.SandboxProxyMap{
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
			ContainerToHostPorts: map[string]string{
				"8080": "32000",
			},
		}

		endpoint, err := ResolveExposedPortEndpoint("10.0.0.1", proxyMap, 8080)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, ExposedPortModeTapIP, endpoint.Mode)
		assert.Equal(t, "192.168.0.2:8080", endpoint.Address)
		assert.Equal(t, int32(0), endpoint.HostPort)
	})

	t.Run("different host uses host port", func(t *testing.T) {
		proxyMap := &proxytypes.SandboxProxyMap{
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
			ContainerToHostPorts: map[string]string{
				"8080": "32000",
			},
		}

		endpoint, err := ResolveExposedPortEndpoint("10.0.0.2", proxyMap, 8080)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, ExposedPortModeHostPort, endpoint.Mode)
		assert.Equal(t, "10.0.0.1:32000", endpoint.Address)
		assert.Equal(t, int32(32000), endpoint.HostPort)
	})

	t.Run("empty caller host falls back to host port", func(t *testing.T) {
		proxyMap := &proxytypes.SandboxProxyMap{
			HostIP: "10.0.0.1",
			ContainerToHostPorts: map[string]string{
				"8080": "32000",
			},
		}

		endpoint, err := ResolveExposedPortEndpoint("", proxyMap, 8080)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, ExposedPortModeHostPort, endpoint.Mode)
		assert.Equal(t, "10.0.0.1:32000", endpoint.Address)
	})

	t.Run("same host without sandbox ip fails", func(t *testing.T) {
		proxyMap := &proxytypes.SandboxProxyMap{
			HostIP: "10.0.0.1",
			ContainerToHostPorts: map[string]string{
				"8080": "32000",
			},
		}

		_, err := ResolveExposedPortEndpoint("10.0.0.1", proxyMap, 8080)
		if !assert.Error(t, err) {
			return
		}
		assert.Contains(t, err.Error(), "sandbox ip is empty")
	})

	t.Run("missing host port mapping fails", func(t *testing.T) {
		proxyMap := &proxytypes.SandboxProxyMap{
			HostIP:    "10.0.0.1",
			SandboxIP: "192.168.0.2",
		}

		_, err := ResolveExposedPortEndpoint("10.0.0.2", proxyMap, 8080)
		if !assert.Error(t, err) {
			return
		}
		assert.Contains(t, err.Error(), "host port mapping")
	})
}

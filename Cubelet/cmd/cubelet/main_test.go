// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
)

func TestEnsureRequiredPluginsAddsCriticalCubeletPlugins(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	cfg.RequiredPlugins = []string{string(constants.InternalPlugin) + "." + constants.StorageID.ID()}

	ensureRequiredPlugins(cfg)

	assert.Contains(t, cfg.RequiredPlugins, string(constants.InternalPlugin)+"."+constants.StorageID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.InternalPlugin)+"."+constants.CubeboxID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.WorkflowPlugin)+"."+constants.WorkflowID.ID())
	assert.Contains(t, cfg.RequiredPlugins, string(constants.CubeboxServicePlugin)+"."+constants.CubeboxServiceID.ID())
}

func TestLoadNetworkPluginBootstrapConfigDecodesNetworkPluginSection(t *testing.T) {
	cfg := platformAgnosticDefaultConfig()
	configPath := filepath.Join("..", "..", "config", "config.toml")

	require.NoError(t, srvconfig.LoadConfig(context.Background(), configPath, cfg))

	networkCfg, ok, err := loadNetworkPluginBootstrapConfig(cfg)
	require.NoError(t, err)
	assert.True(t, ok)
	require.NotNil(t, networkCfg)
	assert.True(t, networkCfg.EnableNetworkAgent)
	assert.Equal(t, "grpc+unix:///tmp/cube/network-agent-grpc.sock", networkCfg.NetworkAgentEndpoint)
}

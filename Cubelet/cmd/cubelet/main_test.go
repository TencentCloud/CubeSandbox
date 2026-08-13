// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
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

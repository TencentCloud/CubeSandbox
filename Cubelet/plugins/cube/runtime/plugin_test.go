// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/version"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestCRIRuntimePluginConfigMigration(t *testing.T) {
	runcSandboxer := "podsandbox"

	grpcCri := map[string]interface{}{
		"enable_selinux":              true,
		"max_container_log_line_size": 100,
		"max_concurrent_downloads":    3,
		"disable_tcp_service":         true,
		"containerd": map[string]interface{}{
			"runtimes": map[string]interface{}{
				"runc": map[string]interface{}{
					"sandbox_mode": runcSandboxer,
				},
			},
		},
	}

	pluginConfigs := map[string]interface{}{
		string(plugins.GRPCPlugin) + ".cri": grpcCri,
	}
	// Use one below the current config version so the migration path is
	// always exercised, matching the real call site that only migrates
	// legacy configs (config.Version < version.ConfigVersion).
	require.NoError(t, configMigration(context.Background(), version.ConfigVersion-1, pluginConfigs))

	runtimeConf, ok := pluginConfigs[string(constants.CubeServicePlugin)+".runtime"].(map[string]interface{})
	require.True(t, ok)
	require.NotNil(t, runtimeConf)
	assert.Equal(t, grpcCri["enable_selinux"], runtimeConf["enable_selinux"])
	assert.Equal(t, grpcCri["max_container_log_line_size"], runtimeConf["max_container_log_line_size"])
	assert.NotContains(t, runtimeConf, "max_concurrent_downloads")
	assert.NotContains(t, runtimeConf, "disable_tcp_service")

	ctd, ok := runtimeConf["containerd"].(map[string]interface{})
	require.True(t, ok)
	require.NotNil(t, ctd)

	runtimes := ctd["runtimes"].(map[string]interface{})
	runc, ok := runtimes["runc"].(map[string]interface{})
	require.True(t, ok)
	require.NotNil(t, runc)
	assert.Equal(t, runcSandboxer, runc["sandboxer"])
	assert.NotContains(t, runc, "sandbox_mode")
}

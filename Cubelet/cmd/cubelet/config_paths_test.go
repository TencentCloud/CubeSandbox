// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
)

func TestConfigDumpArtifactPaths(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "config.toml"))
	require.NoError(t, err)
	for _, tc := range []struct {
		name, extra string
		fail        bool
		defaults    bool
	}{
		{name: "shipped"},
		{name: "defaults", defaults: true},
		{name: "conflicting legacy path", extra: "\n[plugins.\"io.cubelet.cbri.v1.cubebox\"]\nimage_base_path = \"/other/images\"\n", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, append(append([]byte{}, shipped...), []byte(tc.extra)...), 0600))
			output, err := os.CreateTemp(t.TempDir(), "dump")
			require.NoError(t, err)
			original := os.Stdout
			os.Stdout = output
			defer func() { os.Stdout = original; _ = output.Close() }()
			cfg := defaultConfig()
			if !tc.defaults {
				require.NoError(t, srvconfig.LoadConfig(context.Background(), path, cfg))
			}
			err = outputConfig(context.Background(), cfg)
			os.Stdout = original
			if tc.fail {
				require.ErrorContains(t, err, "conflicts with images.image_base_path")
				return
			}
			require.NoError(t, err)
			data, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			var decoded struct {
				Plugins map[string]map[string]interface{} `toml:"plugins"`
			}
			require.NoError(t, toml.Unmarshal(data, &decoded))
			images := decoded.Plugins["io.cubelet.internal.v1.images"]
			require.Equal(t, "/usr/local/services/cubetoolbox", images["cubetool_base_dir"])
			require.Equal(t, "/data/cubelet/cubebox_os_image", images["image_base_path"])
			require.Equal(t, "/usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux", images["shared_kernel_path"])
		})
	}
}

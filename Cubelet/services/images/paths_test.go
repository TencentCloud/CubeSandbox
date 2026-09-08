// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	containerdconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/stretchr/testify/require"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
)

func TestConfiguredArtifactPaths(t *testing.T) {
	for _, tc := range []struct {
		name, config, image, kernel string
		fail                        bool
	}{
		{name: "defaults", image: "/usr/local/services/cubetoolbox/cubebox_os_image", kernel: "/usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux"},
		{name: "separate", config: `[plugins."io.cubelet.internal.v1.images"]
image_base_path = "/data/images"
shared_kernel_path = "/opt/kernel/vmlinux"
`, image: "/data/images", kernel: "/opt/kernel/vmlinux"},
		{name: "legacy aligned", config: `[plugins."io.cubelet.internal.v1.images"]
cubetool_base_dir = "/old"
[plugins."io.cubelet.cbri.v1.cubebox"]
image_base_path = "/old/cubebox_os_image"
kernel_base_path = "/old/cubebox_os_image"
`, image: "/old/cubebox_os_image", kernel: "/old/cube-kernel-scf/vmlinux"},
		{name: "legacy conflict", config: `[plugins."io.cubelet.cbri.v1.cubebox"]
image_base_path = "/other"
`, fail: true},
		{name: "kernel conflict", config: `[plugins."io.cubelet.cbri.v1.cubebox"]
kernel_base_path = "/other"
`, fail: true},
		{name: "legacy base with new image", config: `[plugins."io.cubelet.internal.v1.images"]
cubetool_base_dir = "/opt/tools"
image_base_path = "/data/images"
`, image: "/data/images", kernel: "/opt/tools/cube-kernel-scf/vmlinux"},
		{name: "unrelated cbri installation", config: `[plugins."io.cubelet.cbri.v1.cubebox"]
base_path = "/opt/tools"
snapshot_base_path = "/data/snapshots"
`, image: "/usr/local/services/cubetoolbox/cubebox_os_image", kernel: "/usr/local/services/cubetoolbox/cube-kernel-scf/vmlinux"},
		{name: "relative", config: `[plugins."io.cubelet.internal.v1.images"]
image_base_path = "relative"
`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("version = 2\n"+tc.config), 0600))
			cfg := &srvconfig.Config{Config: &containerdconfig.Config{Version: 3}}
			require.NoError(t, srvconfig.LoadConfig(context.Background(), path, cfg))
			got, err := ResolveConfiguredPaths(context.Background(), cfg)
			if tc.fail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.image, got.ImageBasePath)
			require.Equal(t, tc.kernel, got.SharedKernelPath)
		})
	}
}

func TestConfiguredArtifactPathsAfterImports(t *testing.T) {
	root := t.TempDir()
	imported := filepath.Join(root, "images.toml")
	require.NoError(t, os.WriteFile(imported, []byte(`version = 3
[plugins."io.cubelet.internal.v1.images"]
image_base_path = "/imported/images"
shared_kernel_path = "/installed/vmlinux"
`), 0600))
	configPath := filepath.Join(root, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`version = 3
imports = ["images.toml"]
[plugins."io.cubelet.internal.v1.images"]
image_base_path = "/base/images"
`), 0600))
	cfg := &srvconfig.Config{Config: &containerdconfig.Config{Version: 3}}
	require.NoError(t, srvconfig.LoadConfig(context.Background(), configPath, cfg))
	paths, err := ResolveConfiguredPaths(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, "/imported/images", paths.ImageBasePath)
	require.Equal(t, "/installed/vmlinux", paths.SharedKernelPath)
}

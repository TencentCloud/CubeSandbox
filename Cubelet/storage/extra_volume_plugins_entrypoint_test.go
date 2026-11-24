// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"

	volpkg "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
)

const storagePluginID = "io.cubelet.internal.v1.storage"

// runPatchExtraVolumePlugins runs patch_extra_volume_plugins from the cubelet
// container's entrypoint (deploy/kubernetes/images/scripts) on cfg.
func runPatchExtraVolumePlugins(t *testing.T, cfg, raw string) error {
	t.Helper()
	entry, err := filepath.Abs("../../deploy/kubernetes/images/scripts/component-entrypoint.sh")
	require.NoError(t, err)
	cmd := exec.Command("bash", "-c",
		`source <(awk '/^patch_extra_volume_plugins\(\) \{/,/^}/' "$0") && patch_extra_volume_plugins "$1" "$2"`,
		entry, cfg, raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	t.Logf("entrypoint stderr: %s", stderr.String())
	return err
}

// packagedConfig copies Cubelet's shipped config.toml into a temp dir.
func packagedConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../config/config.toml")
	require.NoError(t, err)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfg, data, 0o644))
	return cfg
}

// The patched file must load through Cubelet's own config path, and the
// volume_plugins entries must decode strictly into PluginConfig.
func TestExtraVolumePluginsEntrypointYieldsLoadableConfig(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cfg := packagedConfig(t)
	require.NoError(t, runPatchExtraVolumePlugins(t, cfg,
		"myfs|rpc|/data/cube-shared/run/myfs/node.sock;tool|binary|/data/cube-shared/bin/cube-volume-tool"))
	// A second start rewrites rather than duplicates.
	require.NoError(t, runPatchExtraVolumePlugins(t, cfg,
		"myfs|rpc|/data/cube-shared/run/myfs/node.sock;tool|binary|/data/cube-shared/bin/cube-volume-tool"))

	var loaded srvconfig.Config
	require.NoError(t, srvconfig.LoadConfig(context.Background(), cfg, &loaded))
	decoded, err := loaded.Decode(context.Background(), storagePluginID, &Config{})
	require.NoError(t, err)
	plugins := decoded.(*Config).VolumePlugins
	require.Equal(t, []volpkg.PluginConfig{
		{Name: "s3", Type: volpkg.PluginTypeBinary, BinaryPath: "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3"},
		{Name: "cos", Type: volpkg.PluginTypeBinary, BinaryPath: "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos"},
		{Name: "myfs", Type: volpkg.PluginTypeRPC, SocketPath: "/data/cube-shared/run/myfs/node.sock"},
		{Name: "tool", Type: volpkg.PluginTypeBinary, BinaryPath: "/data/cube-shared/bin/cube-volume-tool"},
	}, plugins)

	data, err := os.ReadFile(cfg)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, toml.Unmarshal(data, &raw))
	section := raw["plugins"].(map[string]any)[storagePluginID].(map[string]any)
	only, err := toml.Marshal(map[string]any{"volume_plugins": section["volume_plugins"]})
	require.NoError(t, err)
	var strict struct {
		VolumePlugins []volpkg.PluginConfig `toml:"volume_plugins"`
	}
	require.NoError(t, toml.NewDecoder(bytes.NewReader(only)).DisallowUnknownFields().Decode(&strict))
	require.Len(t, strict.VolumePlugins, 4)

	// An empty list restores the shipped file byte for byte.
	require.NoError(t, runPatchExtraVolumePlugins(t, cfg, ""))
	shipped, err := os.ReadFile("../config/config.toml")
	require.NoError(t, err)
	got, err := os.ReadFile(cfg)
	require.NoError(t, err)
	require.Equal(t, string(shipped), string(got))
}

// Every rejected value fails the call and leaves config.toml as shipped.
func TestExtraVolumePluginsEntrypointRejects(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	shipped, err := os.ReadFile("../config/config.toml")
	require.NoError(t, err)
	for name, raw := range map[string]string{
		"form feed escape":   `myfs|rpc|/data/a\form`,
		"tab escape":         `myfs|rpc|/data/a\tb`,
		"invalid escape":     `myfs|rpc|/data/a\qb`,
		"trailing backslash": `myfs|rpc|/data/a\`,
		"real tab":           "myfs|rpc|/data/a\tb",
		"newline":            "myfs|rpc|/data/a\nother|rpc|/data/b",
		"carriage return":    "myfs|rpc|/data/a\r",
		"quote":              `myfs|rpc|/data/a"b`,
		"space":              "myfs|rpc|/data/a b",
		"empty path":         "myfs|rpc|",
		"bad name":           "my fs|rpc|/data/a",
		"bad type":           "myfs|nfs|/data/a",
		"packaged s3":        "s3|binary|/data/a",
		"packaged cos":       "cos|rpc|/data/a",
		"duplicate":          "myfs|rpc|/data/a;myfs|rpc|/data/b",
		"bad second entry":   "myfs|rpc|/data/a;tool|binary|/data/a\\b",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := packagedConfig(t)
			require.Error(t, runPatchExtraVolumePlugins(t, cfg, raw))
			got, err := os.ReadFile(cfg)
			require.NoError(t, err)
			require.Equal(t, string(shipped), string(got))
		})
	}
}

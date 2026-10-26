// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build linux
// +build linux

package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/plugins"

	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/snapshots/overlay/patchoverlay"
)

const (
	capaRemapIDs     = "remap-ids"
	capaOnlyRemapIDs = "only-remap-ids"
)

type Config struct {
	RootPath string `toml:"root_path"`

	DataPath      string `toml:"data_path"`
	UpperdirLabel bool   `toml:"upperdir_label"`
	SyncRemove    bool   `toml:"sync_remove"`

	SlowChown bool `toml:"slow_chown"`

	MountOptions []string `toml:"mount_options"`
}

func defaultConfig() *Config {
	return &Config{
		SyncRemove: true,
		SlowChown:  true,
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.SnapshotPlugin,
		ID:     "overlayfs",
		Config: defaultConfig(),
		Requires: []plugin.Type{
			plugins.MountManagerPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			pluginName := fmt.Sprintf("%v.%v", plugins.SnapshotPlugin, "overlayfs")
			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid overlay configuration")
			}

			var oOpts []patchoverlay.Opt

			oOpts = append(oOpts, patchoverlay.WithUpperdirLabel)
			oOpts = append(oOpts, patchoverlay.WithCubeUseRefPath)
			if !config.SyncRemove {
				oOpts = append(oOpts, patchoverlay.AsynchronousRemove)
			}

			root := ic.Properties[plugins.PropertyRootDir]
			if config.RootPath != "" {
				root = filepath.Join(config.RootPath, pluginName)
			}
			if err := os.MkdirAll(root, 0700); err != nil {
				return nil, fmt.Errorf("failed to create root directory %q: %w", root, err)
			}
			ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
			if err != nil {
				return nil, fmt.Errorf("failed to open metadata store: %w", err)
			}
			oOpts = append(oOpts, patchoverlay.WithMetaStore(ms))

			if len(config.MountOptions) > 0 {
				oOpts = append(oOpts, patchoverlay.WithMountOptions(config.MountOptions))
			}
			if ok, err := overlayutils.SupportsIDMappedMounts(); err == nil && ok {
				oOpts = append(oOpts, patchoverlay.WithRemapIDs)
				ic.Meta.Capabilities = append(ic.Meta.Capabilities, capaRemapIDs)
			}

			if config.SlowChown {
				oOpts = append(oOpts, patchoverlay.WithSlowChown)
			} else {

				ic.Meta.Capabilities = append(ic.Meta.Capabilities, capaOnlyRemapIDs)
			}

			dataPath := root
			if config.DataPath != "" {
				dataPath = filepath.Join(config.DataPath, pluginName)
			}
			ic.Meta.Exports[plugins.SnapshotterRootDir] = dataPath
			overlaySnapshotter, err := patchoverlay.NewSnapshotter(dataPath, oOpts...)
			if err != nil {
				return nil, err
			}

			return overlaySnapshotter, nil
		},
	})
}

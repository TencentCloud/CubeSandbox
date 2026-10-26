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

package transfer

import (
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/containerd/v2/pkg/imageverifier"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	_ "github.com/containerd/containerd/v2/core/transfer/archive"
	_ "github.com/containerd/containerd/v2/core/transfer/image"
	_ "github.com/containerd/containerd/v2/core/transfer/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	local "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/transfer/service"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TransferPlugin,
		ID:   "cube",
		Requires: []plugin.Type{
			plugins.MetadataPlugin,
			plugins.DiffPlugin,
			plugins.ImageVerifierPlugin,
		},
		Config: defaultConfig(),
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			config := ic.Config.(*transferConfig)
			m, err := ic.GetSingle(plugins.MetadataPlugin)
			if err != nil {
				return nil, err
			}
			ms := m.(*metadata.DB)

			var lc local.TransferConfig

			vps, err := ic.GetByType(plugins.ImageVerifierPlugin)
			if err != nil {
				return nil, err
			}
			if len(vps) > 0 {
				lc.Verifiers = make(map[string]imageverifier.ImageVerifier)
				for name, vp := range vps {
					lc.Verifiers[name] = vp.(imageverifier.ImageVerifier)
				}
			}

			lc.MaxConcurrentDownloads = config.MaxConcurrentDownloads
			lc.MaxConcurrentUploadedLayers = config.MaxConcurrentUploadedLayers

			client, err := containerd.New(
				"",
				containerd.WithDefaultNamespace(constants.CubeDefaultNamespace),
				containerd.WithDefaultPlatform(platforms.Default()),
				containerd.WithInMemoryServices(ic),
			)
			if err != nil {
				return nil, fmt.Errorf("unable to init client for cri image service: %w", err)
			}

			if config.UnpackConfiguration == nil {
				config.UnpackConfiguration = defaultUnpackConfig()
			}
			for _, uc := range config.UnpackConfiguration {
				p, err := platforms.Parse(uc.Platform)
				if err != nil {
					return nil, fmt.Errorf("%s: platform configuration %v invalid", plugins.TransferPlugin, uc.Platform)
				}

				sn := client.SnapshotService(uc.Snapshotter)
				if sn == nil {
					return nil, fmt.Errorf("snapshotter %q not found: %w", uc.Snapshotter, errdefs.ErrNotFound)
				}

				var applier diff.Applier
				target := platforms.Only(p)
				if uc.Differ != "" {
					inst, err := ic.GetByID(plugins.DiffPlugin, uc.Differ)
					if err != nil {
						return nil, fmt.Errorf("failed to get instance for diff plugin %q: %w", uc.Differ, err)
					}
					applier = inst.(diff.Applier)
				} else {
					for name, plugin := range ic.GetAll() {
						if plugin.Registration.Type != plugins.DiffPlugin {
							continue
						}
						var matched bool
						for _, p := range plugin.Meta.Platforms {
							if target.Match(p) {
								matched = true
							}
						}
						if !matched {
							continue
						}
						if applier != nil {
							log.G(ic.Context).Warnf("multiple differs match for platform, set `differ` option to choose, skipping %q", plugin.Registration.ID)
							continue
						}
						inst, err := plugin.Instance()
						if err != nil {
							return nil, fmt.Errorf("failed to get instance for diff plugin %q: %w", name, err)
						}
						applier = inst.(diff.Applier)
					}
				}
				if applier == nil {
					return nil, fmt.Errorf("no matching diff plugins: %w", errdefs.ErrNotFound)
				}

				up := unpack.Platform{
					Platform:       target,
					SnapshotterKey: uc.Snapshotter,
					Snapshotter:    sn,
					Applier:        applier,
				}
				lc.UnpackPlatforms = append(lc.UnpackPlatforms, up)
			}
			lc.RegistryConfigPath = config.RegistryConfigPath

			return local.NewTransferService(ms.ContentStore(), metadata.NewImageStore(ms), lc), nil
		},
	})
}

type transferConfig struct {
	MaxConcurrentDownloads int `toml:"max_concurrent_downloads"`

	MaxConcurrentUploadedLayers int `toml:"max_concurrent_uploaded_layers"`

	UnpackConfiguration []unpackConfiguration `toml:"unpack_config,omitempty"`

	RegistryConfigPath string `toml:"config_path"`
}

type unpackConfiguration struct {
	Platform string `toml:"platform"`

	Snapshotter string `toml:"snapshotter"`

	Differ string `toml:"differ"`
}

func defaultConfig() *transferConfig {
	return &transferConfig{
		MaxConcurrentDownloads:      3,
		MaxConcurrentUploadedLayers: 3,
	}
}

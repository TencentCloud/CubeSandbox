// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"errors"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi/chics"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.PluginChi,
		ID:     constants.PluginVSocketManger,
		Config: defaultConfig(),
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			cfg, ok := ic.Config.(*chics.CubeHostFactoryConfig)
			if !ok {
				return nil, errors.New("invalid vsocket config")
			}

			return chics.NewCubeHostClientManager(cfg)
		},
	})
}

const (
	defaultVMRootDir      = "/run/vc/vm"
	defaultCubeSocketName = "cube.sock"
)

func defaultConfig() *chics.CubeHostFactoryConfig {
	return &chics.CubeHostFactoryConfig{
		VMRootDir:         defaultVMRootDir,
		CubeSocketName:    defaultCubeSocketName,
		ProxyPort:         1031,
		CubeHostImagePort: 1029,
	}
}

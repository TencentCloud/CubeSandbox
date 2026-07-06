// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controller

import (
	"fmt"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
)

func init() {
	registerCubeRunTemplateResourceManager()
}

func registerCubeRunTemplateResourceManager() {
	registry.Register(&plugin.Registration{
		Type: constants.ControllerPlugin,
		ID:   constants.PluginRunTemplateManager.ID(),
		Requires: []plugin.Type{
			constants.CubeMetaStorePlugin,
			constants.ControllerConfigPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			obj, err := ic.GetByID(constants.CubeMetaStorePlugin, constants.MultiMetaID.ID())
			if err != nil {
				return nil, fmt.Errorf("unable to get multi meta store: %w", err)
			}
			metaDB := obj.(multimeta.MetadataDBAPI)
			return runtemplate.NewCubeRunTemplateManager(metaDB, nil)
		},
	})
}

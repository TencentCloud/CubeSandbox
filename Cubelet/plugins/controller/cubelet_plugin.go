// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package controller

import (
	"fmt"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
)

func init() {
	registerConfig()
	registerCubelet()
}

func registerConfig() {
	registry.Register(&plugin.Registration{
		Type:   constants.ControllerConfigPlugin,
		ID:     constants.PluginCubelet,
		Config: cubelet.DefaultCubeletConfig(),
		Requires: []plugin.Type{
			plugins.CRIServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			_ = fmt.Sprintf("cubelet/%s", version.Version)
			return ic.Config.(*cubelet.KubeletConfig), nil
		},
	})
}

func registerCubelet() {
	registry.Register(&plugin.Registration{
		Type: constants.ControllerCubeletPlugin,
		ID:   constants.PluginCubelet,
		Requires: []plugin.Type{
			plugins.CRIServicePlugin,
			constants.ControllerPlugin,
			constants.ControllerConfigPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			obj, err := ic.GetByID(constants.ControllerConfigPlugin, constants.PluginCubelet)
			if err != nil {
				return nil, fmt.Errorf("failed to get cubelet config: %w", err)
			}
			cfg := obj.(*cubelet.KubeletConfig)

			obj, err = ic.GetByID(plugins.CRIServicePlugin, "images")
			if err != nil {
				return nil, fmt.Errorf("failed to get cri images service: %w", err)
			}
			cri := obj.(*images.CubeImageService)

			metaCfg := config.GetConfig().MetaServerConfig
			var client *masterclient.Client
			if metaCfg != nil && metaCfg.MetaServerEndpoint != "" {
				client = masterclient.New("http://"+metaCfg.MetaServerEndpoint, tomlext.ToStdTime(cfg.NodeStatusUpdateFrequency))
			}

			var networkAgentClient networkagentclient.Client = networkagentclient.NewNoopClient()
			commonCfg := config.GetConfig().Common
			if commonCfg != nil && commonCfg.EnableNetworkAgent {
				var naErr error
				networkAgentClient, naErr = networkagentclient.NewClient(commonCfg.NetworkAgentEndpoint)
				if naErr != nil {
					log.G(ic.Context).WithError(naErr).Warn("failed to create network-agent client for cubelet")
				}
			}

			var controllerMap = make(map[string]controller.CubeMetaController)
			controllerObjMap, err := ic.GetByType(constants.ControllerPlugin)
			if err != nil {
				return nil, fmt.Errorf("failed to get controller map: %w", err)
			}
			for name, obj := range controllerObjMap {
				c, ok := obj.(controller.CubeMetaController)
				if ok {
					controllerMap[name] = c
				} else {
					log.G(ic.Context).Fatalf("controller %s is not a valid CubeMetaController", name)
				}
			}

			obj, err = ic.GetByID(constants.ControllerPlugin, constants.PluginRunTemplateManager.ID())
			if err != nil {
				return nil, fmt.Errorf("failed to get run template manager: %w", err)
			}
			runtemplateManager := obj.(runtemplate.RunTemplateManager)

			cl, err := cubelet.NewCubelet(
				cfg,
				client,
				controllerMap,
				cri,
				runtemplateManager,
				networkAgentClient,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to create cubelet: %w", err)
			}

			readyHook := ic.RegisterReadiness()
			go func() {
				err := cl.Run(readyHook)
				if err != nil {
					log.G(ic.Context).WithError(err).Fatal("failed to run cubelet")

				}
			}()
			return cl, nil
		},
	})
}

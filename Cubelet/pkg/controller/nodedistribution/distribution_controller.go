// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodedistribution

import "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"

type ControllerConfig struct {
	WorkerNums int `toml:"worker_nums"`
}

func DefaultControllerConfig() *ControllerConfig {
	return &ControllerConfig{WorkerNums: 0}
}

const ControllerName = "cubenodedistribution-controller"

type Controller struct {
	distributionManager distribution.DistributionTaskManager
	config              *ControllerConfig
}

func NewController(_ any, _ any, _ any, _ any, distributionManager distribution.DistributionTaskManager, config *ControllerConfig) *Controller {
	if config == nil {
		config = DefaultControllerConfig()
	}
	return &Controller{
		distributionManager: distributionManager,
		config:              config,
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	<-stopCh
	return nil
}

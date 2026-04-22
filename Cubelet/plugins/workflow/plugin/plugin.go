// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

type flow struct {
	MaxConcurrent int64           `toml:"concurrent" json:"concurrent"`
	Actions       [][]interface{} `toml:"actions" json:"actions"`
}

type Config struct {
	URI   string
	Flows map[string]flow `toml:"flows" json:"flows"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.WorkflowPlugin,
		ID:     constants.WorkflowID.ID(),
		Config: &Config{Flows: map[string]flow{}},
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.CubeStorePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					log.G(context.TODO()).Fatalf("plugin %s init fail:%v", constants.WorkflowID, err.Error())
				}
			}()

			config := ic.Config.(*Config)
			config.URI = fmt.Sprintf("%v.%v", constants.WorkflowPlugin, constants.WorkflowID)
			cubelog.WithFields(cubelog.Fields{"ID": config.URI}).Debugf("config.Flows:%+v", config.Flows)
			mPlugins, err := ic.GetByType(constants.InternalPlugin)
			if err != nil {
				return nil, fmt.Errorf("failed to get internal plugin: %w", err)
			}
			engine := &workflow.Engine{}
			for k, s := range config.Flows {
				flow := &workflow.Workflow{Name: k, MaxConcurrent: s.MaxConcurrent}
				for i, step := range s.Actions {
					steps := &workflow.Step{}
					var names []string
					for _, action := range step {
						name, ok := action.(string)
						if !ok {
							return nil, fmt.Errorf("flow:%v step %v invalid action name(string) %v",
								k, i, action)
						}

						i, ok := mPlugins[name]
						if !ok {
							keys := ""
							for key := range mPlugins {
								keys += key + ","
							}
							return nil, fmt.Errorf("flow:%v step %v no such action(plugin) %v registered of %s",
								k, i, name, keys)
						}

						if err != nil {
							return nil, err
						}
						if i == nil {
							continue
						}
						f, ok := i.(workflow.Flow)
						if !ok {
							return nil, fmt.Errorf("flow:%v step %v action(plugin) %v is not a valid Flow",
								k, i, name)
						}
						names = append(names, f.ID())
						steps.AppendFlow(f)
					}
					if len(steps.Actions) == 0 {
						return nil, fmt.Errorf("flow:%v step %v no actions", k, i)
					}
					steps.Name = strings.Join(names, "#")
					flow.AppendStep(steps)
				}
				if len(flow.Steps) <= 0 {
					return nil, fmt.Errorf("flow:%v no steps", k)
				}

				flow.Limiter = semaphore.NewLimiter(s.MaxConcurrent)
				engine.AddFlow(k, flow)
			}
			i, ok := mPlugins[constants.GCID.ID()]
			if !ok {
				return nil, fmt.Errorf("no such flow %v registered", constants.GCID.ID())
			}
			engine.AddCleanupFlow(i.(workflow.Flow))
			return engine, nil
		},
	})
}

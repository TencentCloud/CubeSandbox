// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimlog

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type Config struct {
	RootPath        string `toml:"root_path"`
	ShimReqLogName  string `toml:"shim_req_log_name"`
	ShimStatLogName string `toml:"shim_stat_log_name"`

	TaskRootPath string `toml:"task_root_path"`
	TmpfsSize    int32  `toml:"tmpfs-size"`
}

var l = &local{}

func init() {
	_ = os.Setenv("CUBELET", "true")
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.ShimLogID.ID(),
		Config: &Config{},

		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l.config = ic.Config.(*Config)
			if l.config.RootPath == "" {
				l.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			_ = os.Setenv("CUBELET_SHIMLOGPATH", l.config.RootPath)

			if l.config.ShimReqLogName == "" {
				l.config.ShimReqLogName = "cube.log"
			}
			if l.config.ShimStatLogName == "" {
				l.config.ShimStatLogName = "cube.stat"
			}
			if l.config.TmpfsSize == 0 {
				l.config.TmpfsSize = 300
			}
			if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
				return nil, fmt.Errorf("init RootPath dir failed, %s", err.Error())
			}

			if l.config.TaskRootPath == "" {
				stateDir := filepath.Dir(l.config.RootPath)
				l.config.TaskRootPath = filepath.Join(stateDir,
					fmt.Sprintf("%s.%s", plugins.RuntimePluginV2, "task"))
			}

			l.shimLogger = CubeLog.GetLogger("shim")
			CubeLog.Debugf("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.ShimLogID), l.config)

			ctx := context.WithValue(ic.Context, CubeLog.KeyCallee, constants.ShimLogID.ID())
			ctx = context.WithValue(ctx, CubeLog.KeyAction, "InitFn")
			if err := l.reload(ctx); err != nil {

				CubeLog.Fatalf("shim log reload fail:%v", err)
			}
			return l, nil
		},
	})
}

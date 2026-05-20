// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type Config struct {
	RootPath string `toml:"root_path"`
	DataPath string `toml:"data_path"`

	DiskSize string `toml:"disksize"`

	WarningPercent int64 `toml:"warningPercent"`

	PoolDefaultFormatSizeList []string `toml:"pool_default_format_size_list"`

	BaseDiskUUID string `toml:"base_disk_uuid"`

	PoolSize int `toml:"pool_size"`

	PoolWorkers int `toml:"pool_worker_num"`

	FAdviseSize int `toml:"fadvise_size"`

	PoolType poolType `toml:"pool_type"`

	PoolTriggerIntervalInMs int `toml:"pool_trigger_interval_in_ms"`

	PoolTriggerBurst int `toml:"pool_trigger_burst"`

	DisableDiskCheck bool `toml:"disable_disk_check"`

	FreeBlocksThreshold int32 `toml:"free_blocks_threshold"`

	FreeInodesThreshold int32            `toml:"free_inodes_threshold"`
	ReconcileInterval   tomlext.Duration `toml:"reconcile_interval"`

	// CmdTimeout overrides the per-command timeout for utils.ExecV
	// invocations in shell.go (cp / truncate / e2fsck / resize2fs /
	// mkfs.ext4). Defaults to defaultCmdTimeout when zero. The slow
	// ext4-create path on multi-GiB images can need noticeably more
	// than the 3s default; this knob lets operators bump it without
	// recompiling.
	CmdTimeout tomlext.Duration `toml:"cmd_timeout"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.StorageID.ID(),
		Config: &Config{},
		Requires: []plugin.Type{
			constants.CubeStorePlugin,
			constants.CubeMetaStorePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			localStorage.config = ic.Config.(*Config)
			if localStorage.config.RootPath == "" {
				localStorage.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}
			if localStorage.config.DataPath == "" {
				localStorage.config.DataPath = localStorage.config.RootPath
			} else {
				localStorage.config.DataPath = filepath.Join(localStorage.config.DataPath,
					fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID))
			}
			if localStorage.config.PoolType == "" {
				localStorage.config.PoolType = cp_type
			}
			if localStorage.config.CmdTimeout == 0 {
				localStorage.config.CmdTimeout = tomlext.FromStdTime(defaultCmdTimeout)
			}
			if tomlext.ToStdTime(localStorage.config.CmdTimeout) < 0 {
				return nil, fmt.Errorf("cmd_timeout must be non-negative, got %v",
					tomlext.ToStdTime(localStorage.config.CmdTimeout))
			}
			cmdTimeout = tomlext.ToStdTime(localStorage.config.CmdTimeout)
			checkPoolType(localStorage.config)

			cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("get cubebox api client fail:%v", err)
			}
			localStorage.cubeboxAPI = cubeboxAPIObj.(cubes.CubeboxAPI)
			CubeLog.Debugf("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.StorageID), localStorage.config)

			if err := localStorage.init(ic); err != nil {
				log.Fatalf("plugin %s init fail:%v", constants.StorageID, err.Error())
				return nil, err
			}

			return localStorage, nil
		},
	})
}

func checkPoolType(c *Config) {
	if c.PoolType == cp_reflink_type {
		baseFormatFile := filepath.Join(c.DataPath, "base.raw")
		targetFile := filepath.Join(c.DataPath, "target.raw")
		defer func() {
			_ = os.RemoveAll(baseFormatFile)
			_ = os.RemoveAll(targetFile)
		}()
		if err := newExt4BaseRaw(baseFormatFile, c.BaseDiskUUID, 512000); err != nil {
			c.PoolType = cp_type
			return
		}

		if err := newExt4RawByReflinkCopy(baseFormatFile, targetFile, 0); err != nil {
			c.PoolType = cp_type
			return
		}
	}
}

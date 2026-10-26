// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

type Config struct {
	RootPath                         string `toml:"root_path"`
	PoolSize                         int    `toml:"pool_size"`
	PoolTriggerIntervalInMs          int    `toml:"pool_trigger_interval_in_ms"`
	VmMemoryOverheadBase             string `toml:"vm_memory_overhead_base"`
	VmMemoryOverheadCoefficient      int64  `toml:"vm_memory_overhead_coefficient"`
	HostMemoryOverheadBase           string `toml:"host_memory_overhead_base"`
	CubeMsgMemoryOverhead            string `toml:"cubemsg_memory_overhead"`
	VmCpuOverhead                    string `toml:"vm_cpu_overhead"`
	HostCpuOverhead                  string `toml:"host_cpu_overhead"`
	DisableCgroupReuse               bool   `toml:"disable_cgroup_reuse"`
	VmSnapshotSpecsConfig            string `toml:"vm_snapshot_specs_config"`
	dynamicDisableMemoryReparentFile bool   `toml:"-"`
	SnapshotDiskDir                  string `toml:"snapshot_disk_dir"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.CgroupID.ID(),
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			l.config = ic.Config.(*Config)
			if l.config.RootPath == "" {
				l.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}
			log.L.Debugf("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.CgroupID), l.config)
			if err := l.init(); err != nil {
				return nil, err
			}
			return l, nil
		},
	})
}

func (c *Config) ShouldSetMemoryReparentFile() bool {
	return !c.dynamicDisableMemoryReparentFile
}

func (c *Config) parseAndSetDynamicConfig(cfg *dynamConf.Config) {
	if cfg == nil || cfg.Common == nil {
		return
	}

	if strings.ToLower(cfg.Common.CgroupDisableMemoryReparentFile) == "true" {
		log.G(context.Background()).Infof("dynamic: cgroup disable memory reparent file")
		c.dynamicDisableMemoryReparentFile = true
	} else {
		log.G(context.Background()).Infof("dynamic: cgroup enable memory reparent file")
		c.dynamicDisableMemoryReparentFile = false
	}
}

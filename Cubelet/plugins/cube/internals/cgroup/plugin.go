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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pooltier"
)

type Config struct {
	RootPath string `toml:"root_path"`
	// PoolSize is the number of cgroup slots to pre-create.
	// Nil or 0 defers to AutoPoolSize.
	PoolSize *int `toml:"pool_size"`
	// AutoPoolSize enables host-based sizing when PoolSize is unset or 0.
	// Nil behaves like true.
	AutoPoolSize                     *bool  `toml:"auto_pool_size"`
	PoolTriggerIntervalInMs          int    `toml:"pool_trigger_interval_in_ms"`
	VmMemoryOverheadBase             string `toml:"vm_memory_overhead_base"`
	VmMemoryOverheadCoefficient      int64  `toml:"vm_memory_overhead_coefficient"`
	HostMemoryOverheadBase           string `toml:"host_memory_overhead_base"`
	CubeMsgMemoryOverhead            string `toml:"cubemsg_memory_overhead"`
	VmCpuOverhead                    string `toml:"vm_cpu_overhead"`
	HostCpuOverhead                  string `toml:"host_cpu_overhead"`
	PoolWorkers                      int    `toml:"pool_workers"`
	VmSnapshotSpecsConfig            string `toml:"vm_snapshot_specs_config"`
	dynamicDisableMemoryReparentFile bool   `toml:"-"`
	SnapshotDiskDir                  string `toml:"snapshot_disk_dir"`

	// EnableNumaAffinity opts into the per-NUMA cgroup pool. When true, every
	// sandbox cgroup is created under numa<N> and inherits that node's
	// cpuset.cpus / cpuset.mems, so the NUMA node chosen at create time binds
	// CPU cores and memory instead of only labelling the sandbox.
	//
	// Default false: an upgrade never changes sandbox placement behind the
	// operator's back. Ignored on single-NUMA hosts, where the v1 pool is kept.
	EnableNumaAffinity bool `toml:"enable_numa_affinity"`
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

// resolvePoolSize returns the cgroup pool size.
// A positive pool_size wins; otherwise auto_pool_size selects a host tier.
// Tiering off falls back to the built-in default. pool_size = 0 defers to
// auto_pool_size.
func (c *Config) resolvePoolSize(logger *log.CubeWrapperLogEntry) (int, error) {
	if c.PoolSize != nil {
		switch {
		case *c.PoolSize < 0:
			return 0, fmt.Errorf("pool_size must not be negative, got %d", *c.PoolSize)
		case *c.PoolSize > 0:
			if c.autoTieringEnabled() {
				logger.Infof("cgroup pool: pool_size=%d is configured explicitly, auto_pool_size is ignored",
					*c.PoolSize)
			} else {
				logger.Infof("cgroup pool: pool_size=%d is configured explicitly", *c.PoolSize)
			}
			return *c.PoolSize, nil
		}
	}

	if !c.autoTieringEnabled() {
		// Tiering off without an explicit size uses the built-in default.
		logger.Warnf("cgroup pool: auto_pool_size is off but pool_size is unset or 0; "+
			"falling back to the built-in default of %d slots, set pool_size explicitly",
			defaultPoolSize)
		return defaultPoolSize, nil
	}

	tier, spec, err := selectTierForHost()
	if err != nil {
		logger.Warnf("cgroup pool: host capacity detection failed (%v), "+
			"using the %s tier: pool_size=%d", err, tier.Name, tier.CgroupPoolSize)
		return tier.CgroupPoolSize, nil
	}
	logger.Infof("cgroup pool: auto_pool_size picked tier=%s (cpus=%d mem_total_mb=%d): pool_size=%d",
		tier.Name, spec.CPUs, spec.MemTotalMB, tier.CgroupPoolSize)
	return tier.CgroupPoolSize, nil
}

var selectTierForHost = pooltier.SelectForHost

// autoTieringEnabled reports whether auto sizing is enabled; unset is enabled.
func (c *Config) autoTieringEnabled() bool {
	return c.AutoPoolSize == nil || *c.AutoPoolSize
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

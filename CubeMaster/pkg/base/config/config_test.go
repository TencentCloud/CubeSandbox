// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package config provides the configuration for the cube master
package config

import (
	"fmt"

	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestInit(t *testing.T) {
	mydir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mydir=%s\n", mydir)
	if os.Getenv("CUBE_MASTER_CONFIG_PATH") == "" {
		configPath := filepath.Clean(filepath.Join(mydir, "../../../test/conf.yaml"))
		if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
			t.Skipf("skip TestInit: config fixture not found: %s", configPath)
		}
		os.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	}
	_, err = Init()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(GetConfig().ExtraConf.BlkQosMap))
	assert.Equal(t, 2, len(GetConfig().ExtraConf.FsQosMap))

	assert.NotNil(t, GetConfig().Scheduler)
	assert.NotNil(t, GetConfig().Scheduler.LargeSizeAffinityConf)
	cubeboxConf := GetConfig().Scheduler.LargeSizeAffinityConf["cubebox"]
	assert.NotNil(t, cubeboxConf)
	assert.Equal(t, true, cubeboxConf.Enable)
	expectMem := resource.MustParse("100Gi")
	gotMem, err := resource.ParseQuantity(cubeboxConf.MemoryLowerWaterMark)
	assert.NoError(t, err)
	assert.True(t, expectMem.Equal(gotMem))
	expectCpu := resource.MustParse("100000m")
	gotCpu, err := resource.ParseQuantity(cubeboxConf.CpuLowerWaterMark)
	assert.NoError(t, err)
	assert.True(t, expectCpu.Equal(gotCpu))
}

func TestNestedLogSettingsUseSnakeCaseOnRoundTrip(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte("log:\n  file_size: 100\n  file_num: 10\n  enable_log_metric: true\n"), &cfg)
	assert.NoError(t, err)
	if !assert.NotNil(t, cfg.Log) {
		return
	}

	assert.Equal(t, 100, cfg.Log.FileSize)
	assert.Equal(t, 10, cfg.Log.FileNum)
	assert.True(t, cfg.Log.EnableLogMetric)

	encoded, err := yaml.Marshal(&cfg)
	assert.NoError(t, err)
	text := string(encoded)
	assert.Contains(t, text, "file_size: 100")
	assert.Contains(t, text, "file_num: 10")
	assert.Contains(t, text, "enable_log_metric: true")
	assert.NotContains(t, text, "fileSize:")
	assert.NotContains(t, text, "fileNum:")
	assert.NotContains(t, text, "enableLogMetric:")
}

func TestGetEffectiveNodeMaxMemReservedInMBFallsBackForSmallNodes(t *testing.T) {
	sconf := &SchedulerConf{
		NodeMaxMemReservedInMB: 10 * 1024,
	}

	got := sconf.GetEffectiveNodeMaxMemReservedInMB("cubebox", 9450)
	assert.Equal(t, int64(945), got)
}

func TestGetEffectiveNodeMaxMemReservedInMBKeepsConfiguredValue(t *testing.T) {
	sconf := &SchedulerConf{
		NodeMaxMemReservedInMB: 512,
	}

	got := sconf.GetEffectiveNodeMaxMemReservedInMB("cubebox", 9450)
	assert.Equal(t, int64(512), got)
}

func TestPreHandleSchedulerIgnoreRedisAllocationDefault(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{}}
	err := preHandleScheduler(cfg)
	assert.NoError(t, err)

	assert.NotNil(t, cfg.Scheduler.IgnoreRedisAllocation)
	assert.False(t, cfg.Scheduler.ShouldIgnoreRedisAllocation())
}

func TestHasDeprecatedOvercommitConfig(t *testing.T) {
	assert.False(t, (&SchedulerConf{}).hasDeprecatedOvercommitConfig())

	assert.True(t, (&SchedulerConf{
		DeprecatedOvercommitRatio: &deprecatedOvercommitRatioConf{CPURatio: 3, MemRatio: 2},
	}).hasDeprecatedOvercommitConfig())

	assert.True(t, (&SchedulerConf{
		DeprecatedOvercommitRatioByType: map[string]deprecatedOvercommitRatioConf{
			"cubebox_gpu": {CPURatio: 1, MemRatio: 1},
		},
	}).hasDeprecatedOvercommitConfig())

	var nilConf *SchedulerConf
	assert.False(t, nilConf.hasDeprecatedOvercommitConfig())
}

func TestEffectiveAllocated(t *testing.T) {
	ignore := false
	sconf := &SchedulerConf{IgnoreRedisAllocation: &ignore}
	assert.Equal(t, int64(1234), sconf.EffectiveAllocated(1234))

	defaultConf := &SchedulerConf{}
	assert.Equal(t, int64(1234), defaultConf.EffectiveAllocated(1234))

	ignoreTrue := true
	ignoring := &SchedulerConf{IgnoreRedisAllocation: &ignoreTrue}
	assert.Equal(t, int64(0), ignoring.EffectiveAllocated(1234))
}

func TestNodeAffinitySelectorAllowedKeySet(t *testing.T) {
	sconf := &SchedulerConf{
		NodeAffinitySelectorAllowedKeys: []string{"gpu"},
	}

	allowed := sconf.NodeAffinitySelectorAllowedKeySet()
	assert.Contains(t, allowed, constants.AffinityKeyZone)
	assert.Contains(t, allowed, constants.AffinityKeyClusterID)
	assert.Contains(t, allowed, constants.AffinityKeyMemorySize)
	assert.Contains(t, allowed, "gpu")
	assert.NotContains(t, allowed, constants.AffinityKeyDisaterRecoverGroup)
}

func TestNodeAffinitySelectorAllowedKeySet_NilReceiver(t *testing.T) {
	var sconf *SchedulerConf
	allowed := sconf.NodeAffinitySelectorAllowedKeySet()
	assert.Contains(t, allowed, constants.AffinityKeyZone)
	assert.Contains(t, allowed, constants.AffinityKeyClusterID)
	assert.Contains(t, allowed, constants.AffinityKeyInstanceType)
	assert.NotContains(t, allowed, "gpu")
}

func TestDefaultNodeAffinitySelectorAllowedKeySet(t *testing.T) {
	allowed := DefaultNodeAffinitySelectorAllowedKeySet()
	assert.Contains(t, allowed, constants.AffinityKeyZone)
	assert.Contains(t, allowed, constants.AffinityKeyClusterID)
	assert.Contains(t, allowed, constants.AffinityKeyCPUType)
	assert.Contains(t, allowed, constants.AffinityKeyMemorySize)
	assert.Contains(t, allowed, constants.AffinityKeyCPUCores)
	assert.Contains(t, allowed, constants.AffinityKeyInstanceType)
	assert.NotContains(t, allowed, "gpu")
	assert.NotContains(t, allowed, constants.AffinityKeyDisaterRecoverGroup)
}

func TestValidateAllowedHostMountPrefixes(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		wantErr  bool
	}{
		{"valid with trailing slash", []string{"/data/shared/"}, false},
		{"valid without trailing slash", []string{"/data/shared"}, false},
		{"multiple valid", []string{"/data/shared/", "/mnt/nfs/"}, false},
		{"reject root path /", []string{"/"}, true},
		{"reject root via traversal", []string{"/data/.."}, true},
		{"reject empty string", []string{""}, true},
		{"reject relative path", []string{"data/shared/"}, true},
		{"reject dot path", []string{"."}, true},
		{"one valid one invalid", []string{"/data/shared/", ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{
				Log: &log.Conf{},
				ExtraConf: &ExtraConf{
					AllowedHostMountPrefixes: tt.prefixes,
				},
			}
			err := validate(c)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetAllowedHostMountPrefixes_Default(t *testing.T) {
	old := cfg
	cfg = nil
	defer func() { cfg = old }()

	got := GetAllowedHostMountPrefixes()
	assert.Equal(t, []string{"/data/shared/"}, got)
}

func TestGetAllowedHostMountPrefixes_AutoAppendSlash(t *testing.T) {
	old := cfg
	cfg = &Config{
		ExtraConf: &ExtraConf{
			AllowedHostMountPrefixes: []string{"/data/shared", "/mnt/nfs/"},
		},
	}
	defer func() { cfg = old }()

	got := GetAllowedHostMountPrefixes()
	assert.Equal(t, []string{"/data/shared/", "/mnt/nfs/"}, got)
}

func TestGetAllowedHostMountPrefixes_DefensiveCopy(t *testing.T) {
	old := cfg
	cfg = &Config{
		ExtraConf: &ExtraConf{
			AllowedHostMountPrefixes: []string{"/data/shared/"},
		},
	}
	defer func() { cfg = old }()

	got := GetAllowedHostMountPrefixes()
	got[0] = "/hacked/"
	// original config must not be affected
	assert.Equal(t, "/data/shared/", cfg.ExtraConf.AllowedHostMountPrefixes[0])
}

func TestGetAllowedHostMountPrefixes_DefaultDefensiveCopy(t *testing.T) {
	old := cfg
	cfg = nil
	defer func() { cfg = old }()

	got := GetAllowedHostMountPrefixes()
	got[0] = "/hacked/"
	// package-level default must not be affected
	got2 := GetAllowedHostMountPrefixes()
	assert.Equal(t, "/data/shared/", got2[0])
}

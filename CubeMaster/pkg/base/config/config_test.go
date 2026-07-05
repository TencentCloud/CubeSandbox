// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package config provides the configuration for the cube master
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
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

func TestPreHandleSchedulerOvercommitAndIgnoreDefaults(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{}}
	err := preHandleScheduler(cfg)
	assert.NoError(t, err)

	assert.NotNil(t, cfg.Scheduler.IgnoreRedisAllocation)
	assert.False(t, cfg.Scheduler.ShouldIgnoreRedisAllocation())

	ratio := cfg.Scheduler.GetEffectiveOvercommitRatio("cubebox")
	assert.Equal(t, 3.0, ratio.CPURatio)
	assert.Equal(t, 2.0, ratio.MemRatio)
}

func TestGetEffectiveOvercommitRatioPrecedence(t *testing.T) {
	sconf := &SchedulerConf{
		OvercommitRatio: &OvercommitRatioConf{CPURatio: 6.0, MemRatio: 4.0},
		OvercommitRatioByType: map[string]OvercommitRatioConf{
			"cubebox_gpu": {CPURatio: 1.0, MemRatio: 1.0},
		},
	}

	// per-type override wins
	gpu := sconf.GetEffectiveOvercommitRatio("cubebox_gpu")
	assert.Equal(t, 1.0, gpu.CPURatio)
	assert.Equal(t, 1.0, gpu.MemRatio)

	// fall back to global ratio
	other := sconf.GetEffectiveOvercommitRatio("cubebox")
	assert.Equal(t, 6.0, other.CPURatio)
	assert.Equal(t, 4.0, other.MemRatio)

	// fall back to built-in default when nothing configured
	empty := &SchedulerConf{}
	def := empty.GetEffectiveOvercommitRatio("cubebox")
	assert.Equal(t, 3.0, def.CPURatio)
	assert.Equal(t, 2.0, def.MemRatio)
}

func TestEffectiveQuotaAndAllocated(t *testing.T) {
	ignore := false
	sconf := &SchedulerConf{
		IgnoreRedisAllocation: &ignore,
		OvercommitRatio:       &OvercommitRatioConf{CPURatio: 6.0, MemRatio: 4.0},
	}

	assert.Equal(t, int64(48000), sconf.EffectiveQuotaCpu("cubebox", 8000))
	assert.Equal(t, int64(64000), sconf.EffectiveQuotaMem("cubebox", 16000))
	// allocation kept when not ignoring
	assert.Equal(t, int64(1234), sconf.EffectiveAllocated(1234))

	// allocation kept by default (not ignoring)
	defaultConf := &SchedulerConf{}
	assert.Equal(t, int64(1234), defaultConf.EffectiveAllocated(1234))

	// allocation zeroed when explicitly ignoring
	ignoreTrue := true
	ignoring := &SchedulerConf{IgnoreRedisAllocation: &ignoreTrue}
	assert.Equal(t, int64(0), ignoring.EffectiveAllocated(1234))
}

func TestOvercommitRatioSanitizesNonPositive(t *testing.T) {
	sconf := &SchedulerConf{
		OvercommitRatio: &OvercommitRatioConf{CPURatio: 0, MemRatio: -1},
	}
	ratio := sconf.GetEffectiveOvercommitRatio("cubebox")
	assert.Equal(t, 3.0, ratio.CPURatio)
	assert.Equal(t, 2.0, ratio.MemRatio)
}

func TestOvercommitRatioSanitizesNaNAndInf(t *testing.T) {
	cases := []OvercommitRatioConf{
		{CPURatio: math.NaN(), MemRatio: math.NaN()},
		{CPURatio: math.Inf(1), MemRatio: math.Inf(1)},
		{CPURatio: math.Inf(-1), MemRatio: math.Inf(-1)},
	}
	for _, c := range cases {
		sconf := &SchedulerConf{OvercommitRatio: &c}
		ratio := sconf.GetEffectiveOvercommitRatio("cubebox")
		assert.Equal(t, 3.0, ratio.CPURatio)
		assert.Equal(t, 2.0, ratio.MemRatio)

		// capacity arithmetic must stay finite after sanitizing
		assert.Equal(t, int64(24000), sconf.EffectiveQuotaCpu("cubebox", 8000))
		assert.Equal(t, int64(32000), sconf.EffectiveQuotaMem("cubebox", 16000))
	}
}

func TestPreHandleSchedulerSanitizesPerTypeRatios(t *testing.T) {
	cfg := &Config{Scheduler: &WrapperSchedulerConf{
		SchedulerConf: SchedulerConf{
			OvercommitRatioByType: map[string]OvercommitRatioConf{
				"bad_zero": {CPURatio: 0, MemRatio: -1},
				"bad_nan":  {CPURatio: math.NaN(), MemRatio: math.Inf(1)},
				"good":     {CPURatio: 8, MemRatio: 5},
			},
		},
	}}
	err := preHandleScheduler(cfg)
	assert.NoError(t, err)

	// malformed per-type ratios are normalized to defaults at init time
	bz := cfg.Scheduler.OvercommitRatioByType["bad_zero"]
	assert.Equal(t, 3.0, bz.CPURatio)
	assert.Equal(t, 2.0, bz.MemRatio)

	bn := cfg.Scheduler.OvercommitRatioByType["bad_nan"]
	assert.Equal(t, 3.0, bn.CPURatio)
	assert.Equal(t, 2.0, bn.MemRatio)

	// valid per-type ratios are preserved
	g := cfg.Scheduler.OvercommitRatioByType["good"]
	assert.Equal(t, 8.0, g.CPURatio)
	assert.Equal(t, 5.0, g.MemRatio)
}

func TestFloatToInt64Clamped(t *testing.T) {
	assert.Equal(t, int64(0), floatToInt64Clamped(math.NaN()))
	assert.Equal(t, int64(math.MaxInt64), floatToInt64Clamped(math.Inf(1)))
	assert.Equal(t, int64(math.MinInt64), floatToInt64Clamped(math.Inf(-1)))
	// overflow beyond int64 range clamps instead of wrapping to garbage
	assert.Equal(t, int64(math.MaxInt64), floatToInt64Clamped(1e30))
	assert.Equal(t, int64(math.MinInt64), floatToInt64Clamped(-1e30))
	// normal values convert (truncate toward zero) as usual
	assert.Equal(t, int64(42), floatToInt64Clamped(42.9))
	assert.Equal(t, int64(0), floatToInt64Clamped(0))
}

func TestEffectiveQuotaClampsOverflow(t *testing.T) {
	// A huge quota combined with a large overcommit ratio must not wrap to a
	// garbage int64; it clamps to MaxInt64 instead.
	sconf := &SchedulerConf{
		OvercommitRatio: &OvercommitRatioConf{CPURatio: 1e6, MemRatio: 1e6},
	}
	assert.Equal(t, int64(math.MaxInt64), sconf.EffectiveQuotaCpu("cubebox", math.MaxInt64))
	assert.Equal(t, int64(math.MaxInt64), sconf.EffectiveQuotaMem("cubebox", math.MaxInt64))
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

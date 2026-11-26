// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pooltier"
)

// stubTierSelect fixes tier selection for tests.
func stubTierSelect(t *testing.T, cpus int, memMB int64) {
	t.Helper()
	orig := selectTierForHost
	t.Cleanup(func() { selectTierForHost = orig })
	selectTierForHost = func() (pooltier.Tier, pooltier.HostSpec, error) {
		return pooltier.Select(cpus, memMB), pooltier.HostSpec{CPUs: cpus, MemTotalMB: memMB}, nil
	}
}

func TestResolvePoolSizeExplicitValueWins(t *testing.T) {
	stubTierSelect(t, 96, 375*1024)
	for _, auto := range []bool{true, false} {
		auto := auto
		cfg := &Config{PoolSize: intPtr(1234), AutoPoolSize: &auto}

		got, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
		require.NoError(t, err)
		require.Equal(t, 1234, got, "auto_pool_size=%v", auto)
	}
}

func TestResolvePoolSizeAutoByDefault(t *testing.T) {
	// Unset auto_pool_size must still tier.
	stubTierSelect(t, 96, 375*1024)
	cfg := &Config{}

	got, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 3000, got)
}

func TestResolvePoolSizeAutoExplicitTrue(t *testing.T) {
	stubTierSelect(t, 128, 750*1024)
	auto := true
	cfg := &Config{AutoPoolSize: &auto}

	got, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 5000, got)
}

func TestResolvePoolSizeAutoWithZeroPoolSize(t *testing.T) {
	// pool_size = 0 defers to auto sizing.
	stubTierSelect(t, 32, 125*1024)
	cfg := &Config{PoolSize: intPtr(0)}

	got, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 1800, got)
}

func TestResolvePoolSizeTieringOffFallsBackToBuiltinDefault(t *testing.T) {
	orig := selectTierForHost
	t.Cleanup(func() { selectTierForHost = orig })
	selectTierForHost = func() (pooltier.Tier, pooltier.HostSpec, error) {
		t.Fatal("host must not be measured when auto_pool_size is off and pool_size is unset")
		return pooltier.Tier{}, pooltier.HostSpec{}, nil
	}
	auto := false
	cfg := &Config{AutoPoolSize: &auto}

	got, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, defaultPoolSize, got)
}

func TestResolvePoolSizeRejectsNegativeValue(t *testing.T) {
	cfg := &Config{PoolSize: intPtr(-1)}

	_, err := cfg.resolvePoolSize(log.GetLogger(context.Background()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be negative")
}

func TestResolvePoolSizeSurvivesHostDetectionFailure(t *testing.T) {
	// Host detection failure must not block startup.
	orig := selectTierForHost
	t.Cleanup(func() { selectTierForHost = orig })
	selectTierForHost = func() (pooltier.Tier, pooltier.HostSpec, error) {
		return pooltier.Select(0, 0), pooltier.HostSpec{}, errors.New("cannot read /proc/meminfo")
	}

	got, err := (&Config{}).resolvePoolSize(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 500, got)
}

func TestConfigTOMLDeserialization(t *testing.T) {
	// Shipped config: auto on, pool_size unset.
	var cfg Config
	require.NoError(t, toml.Unmarshal([]byte(`
auto_pool_size = true
# pool_size = 3000
`), &cfg))
	require.NotNil(t, cfg.AutoPoolSize)
	require.True(t, *cfg.AutoPoolSize)
	require.Nil(t, cfg.PoolSize)

	// Legacy config: pool_size only.
	var legacy Config
	require.NoError(t, toml.Unmarshal([]byte(`
pool_size = 3000
`), &legacy))
	require.Nil(t, legacy.AutoPoolSize)
	require.NotNil(t, legacy.PoolSize)
	require.Equal(t, 3000, *legacy.PoolSize)

	// Explicit pin.
	var pinned Config
	require.NoError(t, toml.Unmarshal([]byte(`
auto_pool_size = false
pool_size = 42
`), &pinned))
	require.False(t, *pinned.AutoPoolSize)
	require.Equal(t, 42, *pinned.PoolSize)
}

func intPtr(v int) *int { return &v }

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

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

func TestResolveTapInitNumExplicitValueWins(t *testing.T) {
	stubTierSelect(t, 96, 375*1024)
	auto := true
	cfg := &Config{TapInitNum: intPtr(1000), AutoTapInitNum: &auto}

	got, err := cfg.resolveTapInitNum(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 1000, got)
}

func TestResolveTapInitNumExplicitZeroDisablesWarmup(t *testing.T) {
	// tap_init_num = 0 disables warmup.
	stubTierSelect(t, 96, 375*1024)
	cfg := &Config{TapInitNum: intPtr(0)}

	got, err := cfg.resolveTapInitNum(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestResolveTapInitNumAutoByDefault(t *testing.T) {
	// Unset auto_tap_init_num must still tier.
	stubTierSelect(t, 96, 375*1024)
	cfg := &Config{}

	got, err := cfg.resolveTapInitNum(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 500, got)
}

func TestResolveTapInitNumAutoTiers(t *testing.T) {
	tests := []struct {
		cpus    int
		memMB   int64
		wantTap int
	}{
		{128, 750 * 1024, 1000},
		{96, 375 * 1024, 500},
		{32, 125 * 1024, 400},
		{16, 31 * 1024, 300},
		{8, 16 * 1024, 200},
	}
	auto := true
	for _, tc := range tests {
		stubTierSelect(t, tc.cpus, tc.memMB)
		got, err := (&Config{AutoTapInitNum: &auto}).resolveTapInitNum(log.GetLogger(context.Background()))
		require.NoError(t, err)
		require.Equal(t, tc.wantTap, got, "cpus=%d memMB=%d", tc.cpus, tc.memMB)
	}
}

func TestResolveTapInitNumTieringOffDisablesWarmup(t *testing.T) {
	orig := selectTierForHost
	t.Cleanup(func() { selectTierForHost = orig })
	selectTierForHost = func() (pooltier.Tier, pooltier.HostSpec, error) {
		t.Fatal("host must not be measured when auto_tap_init_num is off and tap_init_num is unset")
		return pooltier.Tier{}, pooltier.HostSpec{}, nil
	}
	auto := false
	cfg := &Config{AutoTapInitNum: &auto}

	got, err := cfg.resolveTapInitNum(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestResolveTapInitNumRejectsNegativeValue(t *testing.T) {
	cfg := &Config{TapInitNum: intPtr(-1)}

	_, err := cfg.resolveTapInitNum(log.GetLogger(context.Background()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be negative")
}

func TestResolveTapInitNumSurvivesHostDetectionFailure(t *testing.T) {
	orig := selectTierForHost
	t.Cleanup(func() { selectTierForHost = orig })
	selectTierForHost = func() (pooltier.Tier, pooltier.HostSpec, error) {
		return pooltier.Select(0, 0), pooltier.HostSpec{}, errors.New("cannot read /proc/meminfo")
	}

	got, err := (&Config{}).resolveTapInitNum(log.GetLogger(context.Background()))
	require.NoError(t, err)
	require.Equal(t, 200, got, "an unmeasurable host falls to the floor tier, never to 0")
}

func TestTapConfigTOMLDeserialization(t *testing.T) {
	// Shipped config: auto on, tap_init_num unset.
	var cfg Config
	require.NoError(t, toml.Unmarshal([]byte(`
auto_tap_init_num = true
# tap_init_num = 500
`), &cfg))
	require.NotNil(t, cfg.AutoTapInitNum)
	require.True(t, *cfg.AutoTapInitNum)
	require.Nil(t, cfg.TapInitNum)

	// Legacy config: tap_init_num only.
	var legacy Config
	require.NoError(t, toml.Unmarshal([]byte("tap_init_num = 500\n"), &legacy))
	require.Nil(t, legacy.AutoTapInitNum)
	require.Equal(t, 500, *legacy.TapInitNum)

	// Explicit disable: 0 must survive as 0, not nil.
	var disabled Config
	require.NoError(t, toml.Unmarshal([]byte("tap_init_num = 0\n"), &disabled))
	require.NotNil(t, disabled.TapInitNum)
	require.Zero(t, *disabled.TapInitNum)
}

func intPtr(v int) *int { return &v }

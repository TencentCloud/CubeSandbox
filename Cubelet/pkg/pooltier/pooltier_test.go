// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pooltier

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectMatchesReferenceMachines(t *testing.T) {
	// Reference host shapes; memory is reported MiB.
	tests := []struct {
		name     string
		cpus     int
		memMB    int64
		wantTier string
		wantCg   int
		wantTap  int
	}{
		{"16c32g", 16, 31 * 1024, "small", 1000, 300},
		{"32c128g", 32, 125 * 1024, "medium", 1800, 400},
		{"96c384g", 96, 375 * 1024, "large", 3000, 500},
		{"128c768g", 128, 750 * 1024, "xlarge", 5000, 1000},
		{"288c", 288, 1000 * 1024, "xlarge", 5000, 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.cpus, tc.memMB)
			require.Equal(t, tc.wantTier, got.Name)
			require.Equal(t, tc.wantCg, got.CgroupPoolSize)
			require.Equal(t, tc.wantTap, got.TapInitNum)
		})
	}
}

func TestSelectTierBoundaries(t *testing.T) {
	// Bounds are inclusive; memory is in MiB.
	tests := []struct {
		name     string
		cpus     int
		memMB    int64
		wantTier string
	}{
		{"minimal below small cpus", 15, 64 * 1024, "minimal"},
		{"minimal below small memory", 16, 28*1024 - 1, "minimal"},
		{"small at both bounds", 16, 28 * 1024, "small"},
		{"medium below cpus", 31, 128 * 1024, "small"},
		{"medium at both bounds", 32, 115 * 1024, "medium"},
		{"medium memory short", 32, 115*1024 - 1, "small"},
		{"large at both bounds", 96, 345 * 1024, "large"},
		{"large cpu short of xlarge", 127, 1024 * 1024, "large"},
		{"large memory short of xlarge", 128, 690*1024 - 1, "large"},
		{"xlarge at both bounds", 128, 690 * 1024, "xlarge"},
		{"xlarge keeps working above the bound", 288, 1024 * 1024, "xlarge"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantTier, Select(tc.cpus, tc.memMB).Name)
		})
	}
}

func TestSelectUsesShortBoardWhenCPUAndMemoryDisagree(t *testing.T) {
	require.Equal(t, "medium", Select(96, 128*1024).Name)
	require.Equal(t, "small", Select(16, 1024*1024).Name)
}

func TestTierTableIsOrderedLargestFirstAndEndsOpen(t *testing.T) {
	// Table order is the selection policy.
	require.NotEmpty(t, tiers)
	last := tiers[len(tiers)-1]
	require.Zero(t, last.MinCPUs)
	require.Zero(t, last.MinMemMB)
	for i := 1; i < len(tiers); i++ {
		require.Greater(t, tiers[i-1].MinCPUs, tiers[i].MinCPUs, "tier %d must be larger than %d", i-1, i)
		require.Greater(t, tiers[i-1].CgroupPoolSize, tiers[i].CgroupPoolSize)
		require.Greater(t, tiers[i-1].TapInitNum, tiers[i].TapInitNum)
	}
}

func TestParseMemTotalMB(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int64
	}{
		{
			name: "typical",
			data: "MemTotal:       32768 kB\nMemFree:        16384 kB\n",
			want: 32,
		},
		{
			name: "rounds up partial MiB",
			data: "MemFree:        16384 kB\nMemTotal:       39311232 kB\n",
			want: 38390,
		},
		{
			name: "rounds up fractional KiB",
			data: "MemTotal:       1025 kB\n",
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemTotalMB(strings.NewReader(tc.data))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseMemTotalMBRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"missing line", "MemFree: 1024 kB\n"},
		{"truncated line", "MemTotal:\n"},
		{"unparsable value", "MemTotal: lots kB\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMemTotalMB(strings.NewReader(tc.data))
			require.Error(t, err)
		})
	}
}

func TestSelectForHostPicksMatchingTier(t *testing.T) {
	origCPU, origMem := hostCPUCount, readMemTotalMB
	t.Cleanup(func() { hostCPUCount, readMemTotalMB = origCPU, origMem })

	hostCPUCount = func() int { return 32 }
	readMemTotalMB = func() (int64, error) { return 125 * 1024, nil }

	tier, spec, err := SelectForHost()
	require.NoError(t, err)
	require.Equal(t, "medium", tier.Name)
	require.Equal(t, 1800, tier.CgroupPoolSize)
	require.Equal(t, 400, tier.TapInitNum)
	require.Equal(t, int64(125*1024), spec.MemTotalMB)
}

func TestSelectForHostFallsBackToMinimalOnMemoryError(t *testing.T) {
	origCPU, origMem := hostCPUCount, readMemTotalMB
	t.Cleanup(func() { hostCPUCount, readMemTotalMB = origCPU, origMem })

	hostCPUCount = func() int { return 128 }
	readMemTotalMB = func() (int64, error) { return 0, errors.New("boom") }

	tier, spec, err := SelectForHost()
	require.Error(t, err)
	require.Equal(t, 128, spec.CPUs)
	require.Zero(t, spec.MemTotalMB)
	require.Equal(t, "minimal", tier.Name)
}

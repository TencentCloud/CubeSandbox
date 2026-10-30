package v1

import (
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func TestUsageSnapshotFromRawPreservesV1Units(t *testing.T) {
	got := usageSnapshotFromRaw(
		100, 60, 40,
		map[string]uint64{"nr_periods": 7, "nr_throttled": 2, "throttled_time": 9},
		4096, 16384, 3,
		handle.CPULimit{QuotaUS: 200000, PeriodUS: 100000})
	require.Equal(t, uint64(100), got.CPUUsageTotalNS)
	require.Equal(t, uint64(60), got.CPUUserTotalNS)
	require.Equal(t, uint64(40), got.CPUSystemTotalNS)
	require.Equal(t, uint64(9), got.CPUThrottledTotalNS)
	require.Equal(t, uint64(7), got.CPUPeriodsTotal)
	require.Equal(t, uint64(2), got.CPUThrottledPeriodsTotal)
	require.Equal(t, uint64(4096), got.MemoryCurrentBytes)
	require.Equal(t, uint64(16384), got.MemoryLimit.Value)
	require.False(t, got.MemoryLimit.Unlimited)
	require.Equal(t, uint64(3), got.MemoryFailuresTotal)
	require.Equal(t, uint64(200000), got.CPULimit.QuotaUS)
}

func TestUsageSnapshotFromRawToleratesMissingCounters(t *testing.T) {
	// Missing files read as zero values.
	got := usageSnapshotFromRaw(0, 0, 0, nil, 0, 0, 0, handle.CPULimit{})
	require.Zero(t, got.CPUUsageTotalNS)
	require.Zero(t, got.CPUPeriodsTotal)
	require.Zero(t, got.MemoryCurrentBytes)
	require.Zero(t, got.MemoryFailuresTotal)
}

func TestUsageSnapshotFromRawMarksKernelV1UnlimitedMemory(t *testing.T) {
	got := usageSnapshotFromRaw(0, 0, 0, nil, 0, uint64(math.MaxInt64)-4095, 0, handle.CPULimit{})
	require.True(t, got.MemoryLimit.Unlimited)
	require.Zero(t, got.MemoryLimit.Value)
}

func TestUsageSnapshotFromRawKeepsLargeFiniteV1MemoryLimit(t *testing.T) {
	kernelUnlimited := uint64(math.MaxInt64) &^ uint64(os.Getpagesize()-1)
	finiteLimit := kernelUnlimited - uint64(os.Getpagesize())
	got := usageSnapshotFromRaw(0, 0, 0, nil, 0, finiteLimit, 0, handle.CPULimit{})
	require.False(t, got.MemoryLimit.Unlimited)
	require.Equal(t, finiteLimit, got.MemoryLimit.Value)
}

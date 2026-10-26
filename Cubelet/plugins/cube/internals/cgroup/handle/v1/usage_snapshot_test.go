package v1

import (
	"math"
	"os"
	"testing"

	cgroup1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func TestUsageSnapshotFromMetricsPreservesV1Units(t *testing.T) {
	got, err := usageSnapshotFromMetrics(&cgroup1stats.Metrics{
		CPU: &cgroup1stats.CPUStat{
			Usage:      &cgroup1stats.CPUUsage{Total: 100, User: 60, Kernel: 40},
			Throttling: &cgroup1stats.Throttle{Periods: 7, ThrottledPeriods: 2, ThrottledTime: 9},
		},
		Memory:           &cgroup1stats.MemoryStat{Usage: &cgroup1stats.MemoryEntry{Usage: 4096, Limit: 16384, Failcnt: 3}},
		MemoryOomControl: &cgroup1stats.MemoryOomControl{OomKill: 1},
	}, handle.CPULimit{QuotaUS: 200000, PeriodUS: 100000})
	require.NoError(t, err)
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

func TestUsageSnapshotFromMetricsRejectsIncompleteV1Stats(t *testing.T) {
	_, err := usageSnapshotFromMetrics(&cgroup1stats.Metrics{}, handle.CPULimit{})
	require.Error(t, err)
}

func TestUsageSnapshotFromMetricsMarksKernelV1UnlimitedMemory(t *testing.T) {
	got, err := usageSnapshotFromMetrics(&cgroup1stats.Metrics{
		CPU:    &cgroup1stats.CPUStat{Usage: &cgroup1stats.CPUUsage{}, Throttling: &cgroup1stats.Throttle{}},
		Memory: &cgroup1stats.MemoryStat{Usage: &cgroup1stats.MemoryEntry{Limit: uint64(math.MaxInt64) - 4095}},
	}, handle.CPULimit{})
	require.NoError(t, err)
	require.True(t, got.MemoryLimit.Unlimited)
	require.Zero(t, got.MemoryLimit.Value)
}

func TestUsageSnapshotFromMetricsKeepsLargeFiniteV1MemoryLimit(t *testing.T) {
	kernelUnlimited := uint64(math.MaxInt64) &^ uint64(os.Getpagesize()-1)
	finiteLimit := kernelUnlimited - uint64(os.Getpagesize())
	got, err := usageSnapshotFromMetrics(&cgroup1stats.Metrics{
		CPU:    &cgroup1stats.CPUStat{Usage: &cgroup1stats.CPUUsage{}, Throttling: &cgroup1stats.Throttle{}},
		Memory: &cgroup1stats.MemoryStat{Usage: &cgroup1stats.MemoryEntry{Limit: finiteLimit}},
	}, handle.CPULimit{})
	require.NoError(t, err)
	require.False(t, got.MemoryLimit.Unlimited)
	require.Equal(t, finiteLimit, got.MemoryLimit.Value)
}

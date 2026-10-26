package v2

import (
	"math"
	"testing"

	cgroup2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func TestUsageSnapshotFromMetricsConvertsV2MicrosecondsToNanoseconds(t *testing.T) {
	got, err := usageSnapshotFromMetrics(&cgroup2stats.Metrics{
		CPU: &cgroup2stats.CPUStat{
			UsageUsec:     100,
			UserUsec:      60,
			SystemUsec:    40,
			NrPeriods:     7,
			NrThrottled:   2,
			ThrottledUsec: 9,
		},
		Memory:       &cgroup2stats.MemoryStat{Usage: 4096, UsageLimit: math.MaxUint64},
		MemoryEvents: &cgroup2stats.MemoryEvents{Max: 3, Oom: 2, OomKill: 1},
	}, handle.CPULimit{Unlimited: true, PeriodUS: 100000})
	require.NoError(t, err)
	require.Equal(t, uint64(100000), got.CPUUsageTotalNS)
	require.Equal(t, uint64(60000), got.CPUUserTotalNS)
	require.Equal(t, uint64(40000), got.CPUSystemTotalNS)
	require.Equal(t, uint64(9000), got.CPUThrottledTotalNS)
	require.Equal(t, uint64(7), got.CPUPeriodsTotal)
	require.Equal(t, uint64(2), got.CPUThrottledPeriodsTotal)
	require.True(t, got.CPULimit.Unlimited)
	require.True(t, got.MemoryLimit.Unlimited)
	require.Equal(t, uint64(3), got.MemoryFailuresTotal)
}

func TestUsageSnapshotFromMetricsRejectsV2TimeOverflow(t *testing.T) {
	_, err := usageSnapshotFromMetrics(&cgroup2stats.Metrics{
		CPU:    &cgroup2stats.CPUStat{UsageUsec: math.MaxUint64},
		Memory: &cgroup2stats.MemoryStat{},
	}, handle.CPULimit{})
	require.Error(t, err)
}

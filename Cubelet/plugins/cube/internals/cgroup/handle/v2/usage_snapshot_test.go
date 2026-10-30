package v2

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func TestUsageSnapshotFromRawConvertsV2MicrosecondsToNanoseconds(t *testing.T) {
	got, err := usageSnapshotFromRaw(
		map[string]uint64{
			"usage_usec":     100,
			"user_usec":      60,
			"system_usec":    40,
			"nr_periods":     7,
			"nr_throttled":   2,
			"throttled_usec": 9,
		},
		map[string]uint64{"max": 3, "oom": 2, "oom_kill": 1},
		4096, math.MaxUint64,
		handle.CPULimit{Unlimited: true, PeriodUS: 100000})
	require.NoError(t, err)
	require.Equal(t, uint64(100000), got.CPUUsageTotalNS)
	require.Equal(t, uint64(60000), got.CPUUserTotalNS)
	require.Equal(t, uint64(40000), got.CPUSystemTotalNS)
	require.Equal(t, uint64(9000), got.CPUThrottledTotalNS)
	require.Equal(t, uint64(7), got.CPUPeriodsTotal)
	require.Equal(t, uint64(2), got.CPUThrottledPeriodsTotal)
	require.True(t, got.CPULimit.Unlimited)
	require.Equal(t, uint64(4096), got.MemoryCurrentBytes)
	require.True(t, got.MemoryLimit.Unlimited)
	require.Equal(t, uint64(3), got.MemoryFailuresTotal)
}

func TestUsageSnapshotFromRawKeepsFiniteMemoryLimit(t *testing.T) {
	got, err := usageSnapshotFromRaw(nil, nil, 1024, 2048, handle.CPULimit{})
	require.NoError(t, err)
	require.Equal(t, uint64(1024), got.MemoryCurrentBytes)
	require.Equal(t, uint64(2048), got.MemoryLimit.Value)
	require.False(t, got.MemoryLimit.Unlimited)
	require.Equal(t, uint64(0), got.MemoryFailuresTotal)
	require.Equal(t, uint64(0), got.CPUUsageTotalNS)
}

func TestUsageSnapshotFromRawRejectsV2TimeOverflow(t *testing.T) {
	_, err := usageSnapshotFromRaw(
		map[string]uint64{"usage_usec": math.MaxUint64},
		nil, 0, 0, handle.CPULimit{})
	require.Error(t, err)
}

// TestUsageSnapshotReadsOnlyConsumedFiles covers the slim read path against a
// fake cgroupfs tree.
func TestUsageSnapshotReadsOnlyConsumedFiles(t *testing.T) {
	root := t.TempDir()
	group := "/cube_sandbox/sandbox/7"
	dir := filepath.Join(root, group)
	require.NoError(t, os.MkdirAll(dir, 0755))

	files := map[string]string{
		"cpu.stat":       "usage_usec 1250000\nuser_usec 500000\nsystem_usec 750000\nnr_periods 10\nnr_throttled 3\nthrottled_usec 9000\n",
		"cpu.max":        "200000 100000\n",
		"memory.current": "4096\n",
		"memory.max":     "max\n",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
	}
	// memory.events deliberately absent: tolerated as zero.

	h := &handler{root: root, baseGroup: "/cube_sandbox/sandbox"}
	got, err := h.UsageSnapshot(context.Background(), group)
	require.NoError(t, err)
	require.Equal(t, uint64(1250000000), got.CPUUsageTotalNS)
	require.Equal(t, uint64(500000000), got.CPUUserTotalNS)
	require.Equal(t, uint64(750000000), got.CPUSystemTotalNS)
	require.Equal(t, uint64(9000000), got.CPUThrottledTotalNS)
	require.Equal(t, uint64(10), got.CPUPeriodsTotal)
	require.Equal(t, uint64(3), got.CPUThrottledPeriodsTotal)
	require.Equal(t, uint64(200000), got.CPULimit.QuotaUS)
	require.Equal(t, uint64(100000), got.CPULimit.PeriodUS)
	require.Equal(t, uint64(4096), got.MemoryCurrentBytes)
	require.True(t, got.MemoryLimit.Unlimited)
	require.Zero(t, got.MemoryFailuresTotal)

	// A vanished cgroup fails on cpu.max.
	other := &handler{root: root, baseGroup: "/cube_sandbox/sandbox"}
	_, err = other.UsageSnapshot(context.Background(), "/cube_sandbox/sandbox/8")
	require.Error(t, err)
}

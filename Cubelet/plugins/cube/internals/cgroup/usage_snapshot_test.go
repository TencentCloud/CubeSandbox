package cgroup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type usageSnapshotHandle struct {
	handle.Interface
	result handle.UsageSnapshot
	err    error
	errs   []error
	groups []string
}

func (h *usageSnapshotHandle) UsageSnapshot(_ context.Context, group string) (handle.UsageSnapshot, error) {
	h.groups = append(h.groups, group)
	if len(h.errs) > 0 {
		err := h.errs[0]
		h.errs = h.errs[1:]
		return h.result, err
	}
	return h.result, h.err
}

func TestHostMetricsBaselineAtAssignmentReturnsReadFailure(t *testing.T) {
	h := &usageSnapshotHandle{err: errors.New("cgroup stats unavailable")}
	plugin := &CgPlugin{poolV1Handle: h, poolV2Handle: &usageSnapshotHandle{}}

	baseline, err := plugin.hostMetricsBaselineAtAssignment(context.Background(), "/cube_sandbox/sandbox/7")
	require.Nil(t, baseline)
	require.ErrorContains(t, err, "cgroup stats unavailable")
}

func TestHostMetricsBaselineAtAssignmentRetriesTransientReadFailure(t *testing.T) {
	const group = "/cube_sandbox/sandbox/7"
	h := &usageSnapshotHandle{
		result: handle.UsageSnapshot{CPUUsageTotalNS: 101},
		errs:   []error{errors.New("cgroup stats temporarily unavailable"), nil},
	}
	plugin := &CgPlugin{poolV1Handle: h, poolV2Handle: &usageSnapshotHandle{}}

	baseline, err := plugin.hostMetricsBaselineAtAssignment(context.Background(), group)
	require.NoError(t, err)
	require.Equal(t, uint64(101), baseline.CPUUsageTotalNS)
	require.Equal(t, []string{group, group}, h.groups)
}

func TestUsageSnapshotSelectsHandleFromPersistedGroupPath(t *testing.T) {
	v1Handle := &usageSnapshotHandle{result: handle.UsageSnapshot{CPUUsageTotalNS: 1}}
	v2Handle := &usageSnapshotHandle{result: handle.UsageSnapshot{CPUUsageTotalNS: 2}}
	plugin := &CgPlugin{poolV1Handle: v1Handle, poolV2Handle: v2Handle}

	v1, err := plugin.UsageSnapshot(context.Background(), "/cube_sandbox/sandbox/7")
	require.NoError(t, err)
	require.Equal(t, uint64(1), v1.CPUUsageTotalNS)
	require.Equal(t, []string{"/cube_sandbox/sandbox/7"}, v1Handle.groups)
	require.Empty(t, v2Handle.groups)

	v2, err := plugin.UsageSnapshot(context.Background(), "/cube_sandbox_v2/sandbox/numa0/9")
	require.NoError(t, err)
	require.Equal(t, uint64(2), v2.CPUUsageTotalNS)
	require.Equal(t, []string{"/cube_sandbox_v2/sandbox/numa0/9"}, v2Handle.groups)
}

func TestUsageSnapshotRejectsUnknownGroupPath(t *testing.T) {
	plugin := &CgPlugin{poolV1Handle: &usageSnapshotHandle{}, poolV2Handle: &usageSnapshotHandle{}}
	_, err := plugin.UsageSnapshot(context.Background(), "/other/group")
	require.Error(t, err)
}

func TestHostMetricsBaselineAtAssignmentCapturesRawCounters(t *testing.T) {
	const group = "/cube_sandbox/sandbox/7"
	usage := handle.UsageSnapshot{
		CPUUsageTotalNS:          101,
		CPUUserTotalNS:           61,
		CPUSystemTotalNS:         40,
		CPUThrottledTotalNS:      7,
		CPUPeriodsTotal:          11,
		CPUThrottledPeriodsTotal: 3,
		MemoryFailuresTotal:      5,
	}
	h := &usageSnapshotHandle{result: usage}
	plugin := &CgPlugin{poolV1Handle: h, poolV2Handle: &usageSnapshotHandle{}}

	baseline, err := plugin.hostMetricsBaselineAtAssignment(context.Background(), group)
	require.NoError(t, err)
	require.Equal(t, []string{group}, h.groups)
	require.Equal(t, group, baseline.CGroupPath)
	require.Equal(t, usage.CPUUsageTotalNS, baseline.CPUUsageTotalNS)
	require.Equal(t, usage.CPUUserTotalNS, baseline.CPUUserTotalNS)
	require.Equal(t, usage.CPUSystemTotalNS, baseline.CPUSystemTotalNS)
	require.Equal(t, usage.CPUThrottledTotalNS, baseline.CPUThrottledTotalNS)
	require.Equal(t, usage.CPUPeriodsTotal, baseline.CPUPeriodsTotal)
	require.Equal(t, usage.CPUThrottledPeriodsTotal, baseline.CPUThrottledPeriodsTotal)
	require.Equal(t, usage.MemoryFailuresTotal, baseline.MemoryFailuresTotal)
}

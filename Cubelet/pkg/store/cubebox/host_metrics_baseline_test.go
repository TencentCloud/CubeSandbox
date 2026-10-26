package cubebox

import (
	"testing"

	jsoniter "github.com/json-iterator/go"
	"github.com/stretchr/testify/require"
)

func TestHostMetricsBaselineCopyRestoreAndConditionalRollback(t *testing.T) {
	cb := &CubeBox{}
	baseline := &HostMetricsBaseline{
		CGroupPath:          "/cube_sandbox/sandbox/7",
		CPUUsageTotalNS:     100,
		MemoryFailuresTotal: 3,
	}
	cb.RestoreHostMetricsBaseline(baseline)

	copied := cb.HostMetricsBaselineCopy()
	require.Equal(t, baseline, copied)
	require.NotSame(t, baseline, copied)
	copied.CPUUsageTotalNS = 200
	require.Equal(t, uint64(100), cb.HostMetricsBaselineCopy().CPUUsageTotalNS)

	require.False(t, cb.RestoreHostMetricsBaselineIfCurrent(
		HostMetricsBaseline{CGroupPath: baseline.CGroupPath, CPUUsageTotalNS: 999},
		nil,
	))
	require.True(t, cb.RestoreHostMetricsBaselineIfCurrent(*baseline, nil))
	require.Nil(t, cb.HostMetricsBaselineCopy())
}

func TestHostMetricsBaselineJSONRoundTripAndDeepCopy(t *testing.T) {
	cb := &CubeBox{HostMetricsBaselineMissingAtAssignment: true, HostMetricsBaseline: &HostMetricsBaseline{
		CGroupPath:               "/cube_sandbox/sandbox/8",
		CPUUsageTotalNS:          100,
		CPUUserTotalNS:           60,
		CPUSystemTotalNS:         40,
		CPUThrottledTotalNS:      10,
		CPUPeriodsTotal:          8,
		CPUThrottledPeriodsTotal: 2,
		MemoryFailuresTotal:      3,
	}}

	encoded, err := jsoniter.Marshal(cb)
	require.NoError(t, err)
	var decoded CubeBox
	require.NoError(t, jsoniter.Unmarshal(encoded, &decoded))
	require.Equal(t, cb.HostMetricsBaseline, decoded.HostMetricsBaseline)
	require.True(t, decoded.HostMetricsBaselineMissingAtAssignment)

	copied := cb.DeepCopy()
	require.Equal(t, cb.HostMetricsBaseline, copied.HostMetricsBaseline)
	require.NotSame(t, cb.HostMetricsBaseline, copied.HostMetricsBaseline)
	require.True(t, copied.HostMetricsBaselineMissingAtAssignment)
}

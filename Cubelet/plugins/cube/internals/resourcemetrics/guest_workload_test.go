package resourcemetrics

import (
	"math"
	"os"
	"testing"
	"time"

	cgroup1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/typeurl/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNormalizeGuestWorkloadMetricsUsesExplicitBaseline(t *testing.T) {
	baselineRaw := testGuestWorkloadRawSample(testGuestValues{
		cpuTotal: 100, cpuUser: 60, cpuSystem: 40,
		throttledTime: 10, periods: 8, throttledPeriods: 2,
		memoryCurrent: 4096, memoryLimit: 16384, memoryFailures: 3,
	})
	baseline := NewGuestWorkloadCounterBaseline(baselineRaw)
	current := testGuestWorkloadRawSample(testGuestValues{
		cpuTotal: 175, cpuUser: 100, cpuSystem: 75,
		throttledTime: 19, periods: 11, throttledPeriods: 4,
		memoryCurrent: 6144, memoryLimit: 16384, memoryFailures: 5,
	})

	got, err := NormalizeGuestWorkloadMetrics(current, baseline)
	require.NoError(t, err)
	require.Equal(t, uint64(75), got.Counters.CPUUsageTotalNS)
	require.Equal(t, uint64(40), got.Counters.CPUUserTotalNS)
	require.Equal(t, uint64(35), got.Counters.CPUSystemTotalNS)
	require.Equal(t, uint64(9), got.Counters.CPUThrottledTotalNS)
	require.Equal(t, uint64(3), got.Counters.CPUPeriodsTotal)
	require.Equal(t, uint64(2), got.Counters.CPUThrottledPeriodsTotal)
	require.Equal(t, uint64(6144), got.MemoryCurrentBytes)
	require.Equal(t, uint64(16384), got.MemoryLimitBytes)
	require.False(t, got.MemoryLimitUnlimited)
	require.Equal(t, uint64(2), got.Counters.MemoryFailuresTotal)
	require.NotEmpty(t, got.BaselineID)
}

func TestNormalizeGuestWorkloadMetricsRejectsCounterRegression(t *testing.T) {
	baseline := NewGuestWorkloadCounterBaseline(testGuestWorkloadRawSample(testGuestValues{
		cpuTotal: 100, cpuUser: 60, cpuSystem: 40,
		throttledTime: 10, periods: 8, throttledPeriods: 2,
		memoryCurrent: 4096, memoryLimit: 16384, memoryFailures: 3,
	}))
	_, err := NormalizeGuestWorkloadMetrics(
		testGuestWorkloadRawSample(testGuestValues{
			cpuTotal: 99, cpuUser: 60, cpuSystem: 40,
			throttledTime: 10, periods: 8, throttledPeriods: 2,
			memoryCurrent: 4096, memoryLimit: 16384, memoryFailures: 3,
		}),
		baseline,
	)
	require.ErrorContains(t, err, "CPU usage counter regressed")
}

func TestNormalizeGuestWorkloadMetricsRejectsBaselineForAnotherWorkload(t *testing.T) {
	baseline := NewGuestWorkloadCounterBaseline(testGuestWorkloadRawSample(testGuestValues{
		cpuTotal: 100, cpuUser: 60, cpuSystem: 40,
		throttledTime: 10, periods: 8, throttledPeriods: 2,
		memoryCurrent: 4096, memoryLimit: 16384, memoryFailures: 3,
	}))
	current := testGuestWorkloadRawSample(testGuestValues{
		cpuTotal: 175, cpuUser: 100, cpuSystem: 75,
		throttledTime: 19, periods: 11, throttledPeriods: 4,
		memoryCurrent: 6144, memoryLimit: 16384, memoryFailures: 5,
	})
	current.Workload = WorkloadRef{SandboxID: "other-sandbox", ContainerID: "other-container"}

	_, err := NormalizeGuestWorkloadMetrics(current, baseline)
	require.ErrorContains(t, err, "belongs to sandbox/container, not other-sandbox/other-container")
}

func TestNormalizeGuestWorkloadMetricsRejectsEmptyBaselineID(t *testing.T) {
	raw := testGuestWorkloadRawSample(testGuestValues{})
	baseline := NewGuestWorkloadCounterBaseline(raw)
	baseline.ID = ""

	_, err := NormalizeGuestWorkloadMetrics(raw, baseline)
	require.ErrorContains(t, err, "baseline ID is required")
}

func TestDecodeGuestWorkloadMetricRoundTripsStandardPayload(t *testing.T) {
	at := time.Unix(10, 20).UTC()
	payload, err := proto.Marshal(testContainerdGuestMetrics(testGuestValues{
		cpuTotal: 100, cpuUser: 60, cpuSystem: 40,
		throttledTime: 10, periods: 8, throttledPeriods: 2,
		memoryCurrent: 4096, memoryPeak: 8192, memoryLimit: 16384, memoryFailures: 3,
	}))
	require.NoError(t, err)

	workload := WorkloadRef{SandboxID: "sandbox", ContainerID: "container"}
	got, err := DecodeGuestWorkloadMetric(&types.Metric{
		Timestamp: timestamppb.New(at),
		ID:        workload.ContainerID,
		Data: &anypb.Any{
			TypeUrl: cgroupV1MetricsTypeURL,
			Value:   payload,
		},
	}, workload)
	require.NoError(t, err)
	require.Equal(t, at, got.Timestamp)
	require.Equal(t, workload, got.Workload)
	require.Equal(t, uint64(100), got.Counters.CPUUsageTotalNS)
	require.Equal(t, uint64(8), got.Counters.CPUPeriodsTotal)
	require.Equal(t, uint64(4096), got.MemoryCurrentBytes)
	require.Equal(t, uint64(3), got.Counters.MemoryFailuresTotal)
	require.False(t, got.MemoryLimitUnlimited)
}

func TestDecodeGuestWorkloadMetricNormalizesUnlimitedMemoryLimit(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{memoryLimit: math.MaxUint64})

	got, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.NoError(t, err)
	require.True(t, got.MemoryLimitUnlimited)
}

func TestDecodeGuestWorkloadMetricNormalizesKernelV1UnlimitedMemoryLimit(t *testing.T) {
	kernelUnlimited := uint64(math.MaxInt64) &^ uint64(os.Getpagesize()-1)
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{memoryLimit: kernelUnlimited})

	got, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.NoError(t, err)
	require.True(t, got.MemoryLimitUnlimited)
}

func TestDecodeGuestWorkloadMetricKeepsLargeFiniteMemoryLimit(t *testing.T) {
	kernelUnlimited := uint64(math.MaxInt64) &^ uint64(os.Getpagesize()-1)
	finiteLimit := kernelUnlimited - uint64(os.Getpagesize())
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{memoryLimit: finiteLimit})

	got, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.NoError(t, err)
	require.False(t, got.MemoryLimitUnlimited)
	require.Equal(t, finiteLimit, got.MemoryLimitBytes)
}

func TestDecodeGuestWorkloadMetricKeepsZeroMemoryLimitFinite(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{memoryLimit: 0})

	got, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.NoError(t, err)
	require.False(t, got.MemoryLimitUnlimited)
}

func TestGuestWorkloadPayloadUsesContainerdCanonicalTypeURL(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{cpuTotal: 100})

	require.True(t, typeurl.Is(metric.GetData(), (*cgroup1stats.Metrics)(nil)))
	decoded := new(cgroup1stats.Metrics)
	require.NoError(t, typeurl.UnmarshalTo(metric.GetData(), decoded))
	require.Equal(t, uint64(100), decoded.GetCPU().GetUsage().GetTotal())
}

func TestDecodeGuestWorkloadMetricRejectsMismatchedContainerID(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{})

	_, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "other-container"})
	require.ErrorContains(t, err, "does not match container")
}

func TestDecodeGuestWorkloadMetricRejectsMissingTimestamp(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{})
	metric.Timestamp = nil

	_, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.ErrorContains(t, err, "missing a timestamp")
}

func TestDecodeGuestWorkloadMetricRejectsInvalidTimestamp(t *testing.T) {
	metric := testContainerdMetric(t, time.Unix(10, 20).UTC(), testGuestValues{})
	metric.Timestamp = timestamppb.New(time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC))

	_, err := DecodeGuestWorkloadMetric(metric, WorkloadRef{SandboxID: "sandbox", ContainerID: "container"})
	require.ErrorContains(t, err, "invalid timestamp")
}

type testGuestValues struct {
	cpuTotal, cpuUser, cpuSystem             uint64
	throttledTime, periods, throttledPeriods uint64
	memoryCurrent, memoryPeak, memoryLimit   uint64
	memoryFailures                           uint64
}

func testGuestWorkloadRawSample(v testGuestValues) GuestWorkloadRawSample {
	return GuestWorkloadRawSample{
		Timestamp: time.Unix(1, 0).UTC(),
		Workload:  WorkloadRef{SandboxID: "sandbox", ContainerID: "container"},
		Counters: GuestWorkloadCounters{
			CPUUsageTotalNS:          v.cpuTotal,
			CPUUserTotalNS:           v.cpuUser,
			CPUSystemTotalNS:         v.cpuSystem,
			CPUThrottledTotalNS:      v.throttledTime,
			CPUPeriodsTotal:          v.periods,
			CPUThrottledPeriodsTotal: v.throttledPeriods,
			MemoryFailuresTotal:      v.memoryFailures,
		},
		MemoryCurrentBytes: v.memoryCurrent,
		MemoryLimitBytes:   v.memoryLimit,
	}
}

func testContainerdGuestMetrics(v testGuestValues) *cgroup1stats.Metrics {
	return &cgroup1stats.Metrics{
		CPU: &cgroup1stats.CPUStat{
			Usage: &cgroup1stats.CPUUsage{Total: v.cpuTotal, User: v.cpuUser, Kernel: v.cpuSystem},
			Throttling: &cgroup1stats.Throttle{
				Periods:          v.periods,
				ThrottledPeriods: v.throttledPeriods,
				ThrottledTime:    v.throttledTime,
			},
		},
		Memory: &cgroup1stats.MemoryStat{
			Usage: &cgroup1stats.MemoryEntry{
				Usage:   v.memoryCurrent,
				Max:     v.memoryPeak,
				Limit:   v.memoryLimit,
				Failcnt: v.memoryFailures,
			},
		},
	}
}

func testContainerdMetric(t *testing.T, at time.Time, values testGuestValues) *types.Metric {
	t.Helper()
	payload, err := proto.Marshal(testContainerdGuestMetrics(values))
	require.NoError(t, err)
	return &types.Metric{
		Timestamp: timestamppb.New(at),
		ID:        "container",
		Data: &anypb.Any{
			TypeUrl: cgroupV1MetricsTypeURL,
			Value:   payload,
		},
	}
}

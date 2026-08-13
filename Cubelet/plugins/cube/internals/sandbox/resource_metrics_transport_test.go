package sandbox

import (
	"context"
	"errors"
	"testing"

	cgroup1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	"github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/resourcemetrics"
)

type fakeTaskStatsService struct {
	requestedID string
	response    *task.StatsResponse
	err         error
}

func (f *fakeTaskStatsService) Stats(_ context.Context, req *task.StatsRequest) (*task.StatsResponse, error) {
	f.requestedID = req.GetID()
	return f.response, f.err
}

func guestWorkloadSnapshotFromTaskStats(
	ctx context.Context,
	svc taskStatsService,
	sandboxID, containerID string,
	baseline resourcemetrics.GuestWorkloadCounterBaseline,
) (resourcemetrics.GuestWorkloadSnapshot, error) {
	metric, err := metricFromTaskStats(ctx, svc, sandboxID, containerID)
	if err != nil {
		return resourcemetrics.GuestWorkloadSnapshot{}, err
	}
	raw, err := resourcemetrics.DecodeGuestWorkloadMetric(metric, resourcemetrics.WorkloadRef{
		SandboxID:   sandboxID,
		ContainerID: containerID,
	})
	if err != nil {
		return resourcemetrics.GuestWorkloadSnapshot{}, err
	}
	return resourcemetrics.NormalizeGuestWorkloadMetrics(raw, baseline)
}

func TestMetricFromTaskStatsUsesSpecifiedContainerID(t *testing.T) {
	service := &fakeTaskStatsService{response: &task.StatsResponse{
		Stats: &anypb.Any{TypeUrl: "io.containerd.cgroups.v1.Metrics", Value: []byte{1}},
	}}

	got, err := metricFromTaskStats(context.Background(), service, "sandbox", "container")
	require.NoError(t, err)
	require.Equal(t, "container", service.requestedID)
	require.Equal(t, "container", got.GetID())
	require.Same(t, service.response.GetStats(), got.GetData())
}

func TestMetricFromTaskStatsConvertsFailedPreconditionStatus(t *testing.T) {
	service := &fakeTaskStatsService{
		err: status.Error(codes.FailedPrecondition, "guest resource metrics capability is unavailable"),
	}

	_, err := metricFromTaskStats(context.Background(), service, "sandbox", "container")
	require.Error(t, err)
	require.True(t, errdefs.IsFailedPrecondition(err))
	require.True(t, errors.Is(err, errdefs.ErrFailedPrecondition))
}

func TestGuestWorkloadSnapshotFromTaskStatsNormalizesSpecifiedContainer(t *testing.T) {
	payload, err := proto.Marshal(&cgroup1stats.Metrics{
		CPU: &cgroup1stats.CPUStat{
			Usage: &cgroup1stats.CPUUsage{Total: 175, User: 100, Kernel: 75},
			Throttling: &cgroup1stats.Throttle{
				Periods:          11,
				ThrottledPeriods: 4,
				ThrottledTime:    19,
			},
		},
		Memory: &cgroup1stats.MemoryStat{
			Usage: &cgroup1stats.MemoryEntry{Usage: 6144, Limit: 16384, Failcnt: 5},
		},
	})
	require.NoError(t, err)
	service := &fakeTaskStatsService{response: &task.StatsResponse{
		Stats: &anypb.Any{TypeUrl: "io.containerd.cgroups.v1.Metrics", Value: payload},
	}}
	workload := resourcemetrics.WorkloadRef{SandboxID: "sandbox", ContainerID: "container"}
	baseline := resourcemetrics.NewGuestWorkloadCounterBaseline(resourcemetrics.GuestWorkloadRawSample{
		Workload: workload,
		Counters: resourcemetrics.GuestWorkloadCounters{
			CPUUsageTotalNS:          100,
			CPUUserTotalNS:           60,
			CPUSystemTotalNS:         40,
			CPUThrottledTotalNS:      10,
			CPUPeriodsTotal:          8,
			CPUThrottledPeriodsTotal: 2,
			MemoryFailuresTotal:      3,
		},
	})

	got, err := guestWorkloadSnapshotFromTaskStats(context.Background(), service, "sandbox", "container", baseline)
	require.NoError(t, err)
	require.Equal(t, "container", service.requestedID)
	require.Equal(t, workload, got.Workload)
	require.Equal(t, uint64(75), got.Counters.CPUUsageTotalNS)
	require.Equal(t, uint64(40), got.Counters.CPUUserTotalNS)
	require.Equal(t, uint64(35), got.Counters.CPUSystemTotalNS)
	require.Equal(t, uint64(2), got.Counters.MemoryFailuresTotal)
	require.Equal(t, uint64(6144), got.MemoryCurrentBytes)
	require.Equal(t, uint64(16384), got.MemoryLimitBytes)
}

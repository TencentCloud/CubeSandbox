package resourcemetrics

import (
	"fmt"
	"math"
	"time"

	cgroup1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	"github.com/containerd/containerd/api/types"
	"google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

const cgroupV1MetricsTypeURL = "io.containerd.cgroups.v1.Metrics"

type WorkloadRef struct {
	SandboxID   string
	ContainerID string
}

type GuestWorkloadCounters struct {
	CPUUsageTotalNS          uint64
	CPUUserTotalNS           uint64
	CPUSystemTotalNS         uint64
	CPUThrottledTotalNS      uint64
	CPUPeriodsTotal          uint64
	CPUThrottledPeriodsTotal uint64
	MemoryFailuresTotal      uint64
}

type GuestWorkloadRawSample struct {
	Timestamp            time.Time
	Workload             WorkloadRef
	Counters             GuestWorkloadCounters
	CPULimitMillicores   *int64
	MemoryCurrentBytes   uint64
	MemoryLimitBytes     uint64
	MemoryLimitUnlimited bool
}

type GuestWorkloadCounterBaseline struct {
	ID       string
	Workload WorkloadRef
	Counters GuestWorkloadCounters
}

type GuestWorkloadSnapshot struct {
	Timestamp            time.Time
	Workload             WorkloadRef
	BaselineID           string
	Counters             GuestWorkloadCounters
	CPULimitMillicores   *int64
	MemoryCurrentBytes   uint64
	MemoryLimitBytes     uint64
	MemoryLimitUnlimited bool
}

func DecodeGuestWorkloadMetric(metric *types.Metric, workload WorkloadRef) (GuestWorkloadRawSample, error) {
	if workload.SandboxID == "" || workload.ContainerID == "" {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload identity requires sandbox and container IDs")
	}
	if metric == nil || metric.GetData() == nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload metrics for %s/%s are empty", workload.SandboxID, workload.ContainerID)
	}
	if metric.GetID() != workload.ContainerID {
		return GuestWorkloadRawSample{}, fmt.Errorf(
			"guest workload metric ID %q does not match container %q",
			metric.GetID(),
			workload.ContainerID,
		)
	}
	raw := metric.GetData()
	if raw.GetTypeUrl() != cgroupV1MetricsTypeURL {
		return GuestWorkloadRawSample{}, fmt.Errorf("unexpected guest workload metrics type %q", raw.GetTypeUrl())
	}
	metrics := new(cgroup1stats.Metrics)
	if err := proto.Unmarshal(raw.GetValue(), metrics); err != nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("decode guest workload metrics for %s/%s: %w", workload.SandboxID, workload.ContainerID, err)
	}
	cpu, memory := metrics.GetCPU(), metrics.GetMemory()
	if cpu == nil || cpu.GetUsage() == nil || cpu.GetThrottling() == nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload CPU metrics for %s/%s are incomplete", workload.SandboxID, workload.ContainerID)
	}
	if memory == nil || memory.GetUsage() == nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload memory metrics for %s/%s are incomplete", workload.SandboxID, workload.ContainerID)
	}

	timestampProto := metric.GetTimestamp()
	if timestampProto == nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload metrics for %s/%s are missing a timestamp", workload.SandboxID, workload.ContainerID)
	}
	if err := timestampProto.CheckValid(); err != nil {
		return GuestWorkloadRawSample{}, fmt.Errorf("guest workload metrics for %s/%s have an invalid timestamp: %w", workload.SandboxID, workload.ContainerID, err)
	}
	timestamp := timestampProto.AsTime()
	usage, throttling, memoryUsage := cpu.GetUsage(), cpu.GetThrottling(), memory.GetUsage()
	memoryLimit := memoryUsage.GetLimit()
	return GuestWorkloadRawSample{
		Timestamp: timestamp,
		Workload:  workload,
		Counters: GuestWorkloadCounters{
			CPUUsageTotalNS:          usage.GetTotal(),
			CPUUserTotalNS:           usage.GetUser(),
			CPUSystemTotalNS:         usage.GetKernel(),
			CPUThrottledTotalNS:      throttling.GetThrottledTime(),
			CPUPeriodsTotal:          throttling.GetPeriods(),
			CPUThrottledPeriodsTotal: throttling.GetThrottledPeriods(),
			MemoryFailuresTotal:      memoryUsage.GetFailcnt(),
		},
		MemoryCurrentBytes:   memoryUsage.GetUsage(),
		MemoryLimitBytes:     memoryLimit,
		MemoryLimitUnlimited: guestMemoryLimitUnlimited(memoryLimit),
	}, nil
}

func NewGuestWorkloadCounterBaseline(raw GuestWorkloadRawSample) GuestWorkloadCounterBaseline {
	return GuestWorkloadCounterBaseline{
		ID:       fmt.Sprintf("%s/%s/%d", raw.Workload.SandboxID, raw.Workload.ContainerID, raw.Timestamp.UnixNano()),
		Workload: raw.Workload,
		Counters: raw.Counters,
	}
}

func NormalizeGuestWorkloadMetrics(raw GuestWorkloadRawSample, baseline GuestWorkloadCounterBaseline) (GuestWorkloadSnapshot, error) {
	if baseline.ID == "" {
		return GuestWorkloadSnapshot{}, fmt.Errorf("guest workload baseline ID is required")
	}
	if raw.Workload != baseline.Workload {
		return GuestWorkloadSnapshot{}, fmt.Errorf(
			"guest workload baseline %q belongs to %s/%s, not %s/%s",
			baseline.ID,
			baseline.Workload.SandboxID,
			baseline.Workload.ContainerID,
			raw.Workload.SandboxID,
			raw.Workload.ContainerID,
		)
	}
	counters, err := subtractGuestCounters(raw.Counters, baseline.Counters)
	if err != nil {
		return GuestWorkloadSnapshot{}, err
	}
	return GuestWorkloadSnapshot{
		Timestamp:            raw.Timestamp,
		Workload:             raw.Workload,
		BaselineID:           baseline.ID,
		Counters:             counters,
		CPULimitMillicores:   cloneInt64(raw.CPULimitMillicores),
		MemoryCurrentBytes:   raw.MemoryCurrentBytes,
		MemoryLimitBytes:     raw.MemoryLimitBytes,
		MemoryLimitUnlimited: raw.MemoryLimitUnlimited,
	}, nil
}

func guestMemoryLimitUnlimited(limit uint64) bool {
	return limit == math.MaxUint64 || handle.IsCgroupV1UnlimitedMemoryLimit(limit)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func subtractGuestCounters(current, baseline GuestWorkloadCounters) (GuestWorkloadCounters, error) {
	var result GuestWorkloadCounters
	values := []struct {
		name              string
		current, baseline uint64
		target            *uint64
	}{
		{name: "CPU usage", current: current.CPUUsageTotalNS, baseline: baseline.CPUUsageTotalNS, target: &result.CPUUsageTotalNS},
		{name: "CPU user", current: current.CPUUserTotalNS, baseline: baseline.CPUUserTotalNS, target: &result.CPUUserTotalNS},
		{name: "CPU system", current: current.CPUSystemTotalNS, baseline: baseline.CPUSystemTotalNS, target: &result.CPUSystemTotalNS},
		{name: "CPU throttled time", current: current.CPUThrottledTotalNS, baseline: baseline.CPUThrottledTotalNS, target: &result.CPUThrottledTotalNS},
		{name: "CPU periods", current: current.CPUPeriodsTotal, baseline: baseline.CPUPeriodsTotal, target: &result.CPUPeriodsTotal},
		{name: "CPU throttled periods", current: current.CPUThrottledPeriodsTotal, baseline: baseline.CPUThrottledPeriodsTotal, target: &result.CPUThrottledPeriodsTotal},
		{name: "memory failures", current: current.MemoryFailuresTotal, baseline: baseline.MemoryFailuresTotal, target: &result.MemoryFailuresTotal},
	}

	for _, value := range values {
		if value.current < value.baseline {
			return GuestWorkloadCounters{}, fmt.Errorf("guest workload %s counter regressed from baseline %d to %d", value.name, value.baseline, value.current)
		}
		*value.target = value.current - value.baseline
	}
	return result, nil
}

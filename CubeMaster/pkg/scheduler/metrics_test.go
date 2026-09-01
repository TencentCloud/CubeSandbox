// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	return testutil.ToFloat64(vec.WithLabelValues(labels...))
}

func histogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%v): %v", labels, err)
	}
	h, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("metric %v is not a histogram", labels)
	}
	m := &dto.Metric{}
	if err := h.Write(m); err != nil {
		t.Fatalf("histogram Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestObserveScheduleAttempt(t *testing.T) {
	successBefore := counterValue(t, schedulerAttempts, DefaultProfile, metricResultSuccess, attemptReasonNone)
	noNodeBefore := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonNoNode)
	prefilterBefore := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonPreFilter)
	errorBefore := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonError)
	histBefore := histogramCount(t, schedulerDuration, DefaultProfile)

	ObserveScheduleAttempt(DefaultProfile, nil, time.Millisecond)
	ObserveScheduleAttempt(DefaultProfile, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no res"), 2*time.Millisecond)
	ObserveScheduleAttempt(DefaultProfile, ret.Err(errorcode.ErrorCode_SelectNodesFailed, "prefilter"), 3*time.Millisecond)
	ObserveScheduleAttempt(DefaultProfile, ret.Err(errorcode.ErrorCode_MasterInternalError, "boom"), 4*time.Millisecond)
	ObserveScheduleAttempt(DefaultProfile, errors.New("plain"), 5*time.Millisecond)

	if got := counterValue(t, schedulerAttempts, DefaultProfile, metricResultSuccess, attemptReasonNone); got != successBefore+1 {
		t.Fatalf("success attempts delta = %v, want 1", got-successBefore)
	}
	if got := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonNoNode); got != noNodeBefore+1 {
		t.Fatalf("no_node attempts delta = %v, want 1", got-noNodeBefore)
	}
	if got := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonPreFilter); got != prefilterBefore+1 {
		t.Fatalf("prefilter attempts delta = %v, want 1", got-prefilterBefore)
	}
	// MasterInternalError and a plain error both land in the generic bucket.
	if got := counterValue(t, schedulerAttempts, DefaultProfile, metricResultError, attemptReasonError); got != errorBefore+2 {
		t.Fatalf("error attempts delta = %v, want 2", got-errorBefore)
	}
	if got := histogramCount(t, schedulerDuration, DefaultProfile); got != histBefore+5 {
		t.Fatalf("duration histogram count delta = %v, want 5", got-histBefore)
	}
}

func TestRecordDecision(t *testing.T) {
	hitBefore := counterValue(t, schedulerDecisions, DefaultProfile, "true")
	missBefore := counterValue(t, schedulerDecisions, DefaultProfile, "false")

	RecordDecision(DefaultProfile, true)
	RecordDecision(DefaultProfile, false)
	RecordDecision(DefaultProfile, false)

	if got := counterValue(t, schedulerDecisions, DefaultProfile, "true"); got != hitBefore+1 {
		t.Fatalf("template_hit=true delta = %v, want 1", got-hitBefore)
	}
	if got := counterValue(t, schedulerDecisions, DefaultProfile, "false"); got != missBefore+2 {
		t.Fatalf("template_hit=false delta = %v, want 2", got-missBefore)
	}
}

func TestRecordReschedule(t *testing.T) {
	// rescheduleReason consults the errorcode retry maps; initialize them
	// with an empty config the same way main does.
	errorcode.InitCubeCodeRetryMap(&config.Config{CubeletConf: &config.CubeletConf{}})

	rpcBefore := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonRPCError)
	circuitBefore := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonCircuitBreak)
	admissionBefore := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonAdmission)
	otherBefore := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonOther)

	RecordReschedule(DefaultProfile, errorcode.ErrorCode_ReqCubeAPIFailed)
	RecordReschedule(DefaultProfile, errorcode.ErrorCode_ConnHostFailed)
	RecordReschedule(DefaultProfile, errorcode.ErrorCode_SelectNodesFailed)
	RecordReschedule(DefaultProfile, errorcode.ErrorCode_DBError)

	if got := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonRPCError); got != rpcBefore+1 {
		t.Fatalf("rpc_error delta = %v, want 1", got-rpcBefore)
	}
	if got := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonCircuitBreak); got != circuitBefore+1 {
		t.Fatalf("circuit_break delta = %v, want 1", got-circuitBefore)
	}
	if got := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonAdmission); got != admissionBefore+1 {
		t.Fatalf("admission delta = %v, want 1", got-admissionBefore)
	}
	if got := counterValue(t, schedulerReschedules, DefaultProfile, rescheduleReasonOther); got != otherBefore+1 {
		t.Fatalf("other delta = %v, want 1", got-otherBefore)
	}
}

func TestObserveSandboxCreate(t *testing.T) {
	successBefore := counterValue(t, sandboxCreateAttempts, DefaultProfile, metricResultSuccess)
	errorBefore := counterValue(t, sandboxCreateAttempts, DefaultProfile, metricResultError)
	successHistBefore := histogramCount(t, sandboxCreateDuration, DefaultProfile, metricResultSuccess)
	errorHistBefore := histogramCount(t, sandboxCreateDuration, DefaultProfile, metricResultError)

	start := time.Now()
	ObserveSandboxCreate(DefaultProfile, true, start, start.Add(1500*time.Millisecond))
	// Early-return path: endTime never set.
	ObserveSandboxCreate(DefaultProfile, false, start, time.Time{})

	if got := counterValue(t, sandboxCreateAttempts, DefaultProfile, metricResultSuccess); got != successBefore+1 {
		t.Fatalf("create success delta = %v, want 1", got-successBefore)
	}
	if got := counterValue(t, sandboxCreateAttempts, DefaultProfile, metricResultError); got != errorBefore+1 {
		t.Fatalf("create error delta = %v, want 1", got-errorBefore)
	}
	if got := histogramCount(t, sandboxCreateDuration, DefaultProfile, metricResultSuccess); got != successHistBefore+1 {
		t.Fatalf("create success duration count delta = %v, want 1", got-successHistBefore)
	}
	if got := histogramCount(t, sandboxCreateDuration, DefaultProfile, metricResultError); got != errorHistBefore+1 {
		t.Fatalf("create error duration count delta = %v, want 1", got-errorHistBefore)
	}
}

// identityCapFn resolves capacity as raw quota (no overcommit) and accounted
// allocation as raw usage.
func identityCapFn(n nodeResourceStat) (int64, int64, int64, int64) {
	return n.quotaCpuMilli, n.quotaMemMB, n.cpuUsageMilli, n.memUsageMB
}

func TestSumClusterQuota(t *testing.T) {
	nodes := []nodeResourceStat{
		{quotaCpuMilli: 1000, quotaMemMB: 2048, cpuUsageMilli: 100, memUsageMB: 512},
		{quotaCpuMilli: 2000, quotaMemMB: 1024},
	}
	overcommitFn := func(n nodeResourceStat) (int64, int64, int64, int64) {
		return n.quotaCpuMilli * 3, n.quotaMemMB * 2, n.cpuUsageMilli, n.memUsageMB
	}

	q := sumClusterQuota(nodes, overcommitFn)
	if q.cpuAllocated != 100 || q.cpuCapacity != 9000 {
		t.Fatalf("cpu quota = (%v, %v), want (100, 9000)", q.cpuAllocated, q.cpuCapacity)
	}
	if q.memAllocated != 512 || q.memCapacity != 6144 {
		t.Fatalf("mem quota = (%v, %v), want (512, 6144)", q.memAllocated, q.memCapacity)
	}

	if got := sumClusterQuota(nil, identityCapFn); got != (clusterQuota{}) {
		t.Fatalf("empty nodes quota = %+v, want zero", got)
	}
}

func TestNodeLoadCV(t *testing.T) {
	nodes := []nodeResourceStat{
		{quotaCpuMilli: 1000, quotaMemMB: 2048, cpuUsageMilli: 500, memUsageMB: 512},
		{quotaCpuMilli: 1000, quotaMemMB: 2048, cpuUsageMilli: 250, memUsageMB: 1024},
		// Zero mem quota must be skipped, not skew the CV towards 0.
		{quotaCpuMilli: 1000, quotaMemMB: 0, cpuUsageMilli: 750, memUsageMB: 4096},
	}

	cpuCV, memCV := nodeLoadCV(nodes)
	// cpu ratios 0.5 / 0.25 / 0.75: mean 0.5, population stddev sqrt(1/24).
	wantCPU := math.Sqrt(1.0/24.0) / 0.5
	if math.Abs(cpuCV-wantCPU) > 1e-9 {
		t.Fatalf("cpu CV = %v, want %v", cpuCV, wantCPU)
	}
	// mem ratios 0.25 / 0.5: mean 0.375, population stddev 0.125.
	wantMem := 0.125 / 0.375
	if math.Abs(memCV-wantMem) > 1e-9 {
		t.Fatalf("mem CV = %v, want %v", memCV, wantMem)
	}

	if cpuCV, memCV := nodeLoadCV(nil); cpuCV != 0 || memCV != 0 {
		t.Fatalf("empty nodes CV = (%v, %v), want (0, 0)", cpuCV, memCV)
	}
	// All-idle nodes: zero mean must not divide by zero.
	idle := []nodeResourceStat{{quotaCpuMilli: 1000, quotaMemMB: 2048}}
	if cpuCV, memCV := nodeLoadCV(idle); cpuCV != 0 || memCV != 0 {
		t.Fatalf("idle nodes CV = (%v, %v), want (0, 0)", cpuCV, memCV)
	}
}

func TestCountActiveEmptyNodes(t *testing.T) {
	nodes := []nodeResourceStat{
		{mvmNum: 2},
		{cpuUsageMilli: 1},
		{memUsageMB: 1},
		{},
	}
	active, empty := countActiveEmptyNodes(nodes)
	if active != 3 || empty != 1 {
		t.Fatalf("active/empty = (%d, %d), want (3, 1)", active, empty)
	}
	if active, empty := countActiveEmptyNodes(nil); active != 0 || empty != 0 {
		t.Fatalf("nil nodes active/empty = (%d, %d), want (0, 0)", active, empty)
	}
}

func TestFragmentedCapacityRatio(t *testing.T) {
	// Shape 2000m / 2048MB against identity capacity.
	// A fits; B is short on cpu; C is short on mem.
	nodes := []nodeResourceStat{
		{quotaCpuMilli: 4000, quotaMemMB: 4096, cpuUsageMilli: 1000, memUsageMB: 1024}, // free 3000m/3072MB
		{quotaCpuMilli: 4000, quotaMemMB: 4096, cpuUsageMilli: 3500, memUsageMB: 1024}, // free 500m/3072MB
		{quotaCpuMilli: 4000, quotaMemMB: 4096, cpuUsageMilli: 0, memUsageMB: 4000},    // free 4000m/96MB
	}

	got := fragmentedCapacityRatio(nodes, identityCapFn, 2000, 2048)
	// fragCpu = (500+4000)/7500 = 0.6; fragMem = (3072+96)/6240.
	want := (0.6 + 3168.0/6240.0) / 2
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("fragmented ratio = %v, want %v", got, want)
	}

	// Every node fits the shape -> no fragmentation.
	roomy := []nodeResourceStat{{quotaCpuMilli: 8000, quotaMemMB: 8192, cpuUsageMilli: 1000}}
	if got := fragmentedCapacityRatio(roomy, identityCapFn, 2000, 2048); got != 0 {
		t.Fatalf("all-fit fragmented ratio = %v, want 0", got)
	}

	// No nodes (or no free capacity) -> 0, never NaN.
	if got := fragmentedCapacityRatio(nil, identityCapFn, 2000, 2048); got != 0 {
		t.Fatalf("empty fragmented ratio = %v, want 0", got)
	}
	full := []nodeResourceStat{{quotaCpuMilli: 4000, quotaMemMB: 4096, cpuUsageMilli: 4000, memUsageMB: 4096}}
	if got := fragmentedCapacityRatio(full, identityCapFn, 2000, 2048); got != 0 {
		t.Fatalf("fully-allocated fragmented ratio = %v, want 0", got)
	}
}

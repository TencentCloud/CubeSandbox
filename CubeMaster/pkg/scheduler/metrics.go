// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// DefaultProfile is the scheduling profile label attached to every metric in
// this file.
// TODO(profile-router): pass the real profile name once the module-1 Profile
// Router lands; until then all series carry profile="default".
const DefaultProfile = "default"

// Label value enums. Cardinality is intentionally kept small and closed.
const (
	metricResultSuccess = "success"
	metricResultError   = "error"

	// attemptReasonNone marks successful attempts; the reason label is always
	// present so series stay queryable with a fixed label set.
	attemptReasonNone = "none"
	// attemptReasonNoNode maps ErrorCode_SelectNodesNoRes: a stage ended with
	// an empty candidate set (preFilter/filter/score/backoff all converge on
	// this code).
	attemptReasonNoNode = "no_node"
	// attemptReasonPreFilter maps ErrorCode_SelectNodesFailed: the
	// pre-selector (or backoff selector) call itself failed.
	attemptReasonPreFilter = "prefilter"
	// attemptReasonError covers everything else (panic recovery, internal
	// errors, ...).
	attemptReasonError = "error"
)

// Reschedule reason categories for handleCubelet retries, derived from the
// cubelet/master error code. The classification order in rescheduleReason is
// significant because a code can sit in several retry maps at once:
// rpc_error > circuit_break > reuse > loop_retry > backoff > admission > other.
const (
	rescheduleReasonRPCError     = "rpc_error"
	rescheduleReasonCircuitBreak = "circuit_break"
	rescheduleReasonReuse        = "reuse"
	rescheduleReasonLoopRetry    = "loop_retry"
	rescheduleReasonBackoff      = "backoff"
	rescheduleReasonAdmission    = "admission"
	rescheduleReasonOther        = "other"
)

const (
	resourceLabelCPU    = "cpu"
	resourceLabelMem    = "mem"
	quotaTypeAllocated  = "allocated"
	quotaTypeCapacity   = "capacity"
	templateHitLabel    = "template_hit"
	profileLabel        = "profile"
	resultLabel         = "result"
	reasonLabel         = "reason"
	resourceLabel       = "resource"
	quotaTypeLabel      = "type"
	shapeLabel          = "shape"
	fragmentShapeMaxMVM = "max_mvm"
)

// clusterGaugeCollectInterval is how often the cluster-level gauges are
// re-computed from localcache.
const clusterGaugeCollectInterval = 15 * time.Second

var (
	schedulerAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scheduler_attempts_total",
		Help: "Total scheduler Select attempts, by profile, result and failure reason.",
	}, []string{profileLabel, resultLabel, reasonLabel})

	schedulerDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "scheduler_duration_seconds",
		Help:    "Latency of a single scheduler Select call, by profile.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 13), // 0.5ms .. ~2s
	}, []string{profileLabel})

	schedulerDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scheduler_decisions_total",
		Help: "Total successful scheduling decisions, by profile and whether the selected node already holds a local replica of the requested template.",
	}, []string{profileLabel, templateHitLabel})

	schedulerReschedules = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "scheduler_reschedules_total",
		Help: "Total handleCubelet retry/reschedule events during sandbox create, by profile and error-code category.",
	}, []string{profileLabel, reasonLabel})

	sandboxCreateAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_create_attempts_total",
		Help: "Total sandbox create requests, by profile and result.",
	}, []string{profileLabel, resultLabel})

	sandboxCreateDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sandbox_create_duration_seconds",
		Help:    "End-to-end latency from sandbox create acceptance to completion, by profile and result.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 13), // 100ms .. ~7min
	}, []string{profileLabel, resultLabel})

	// clusterQuotaGauge exposes summed cluster resources: allocated is the
	// usage the scheduler accounts for (EffectiveAllocated), capacity is the
	// overcommitted quota (quota * overcommit_ratio). cpu in millicores,
	// mem in MB.
	clusterQuotaGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scheduler_cluster_quota",
		Help: "Summed cluster quota seen by the scheduler (cpu in millicores, mem in MB), by resource and type (allocated/capacity).",
	}, []string{resourceLabel, quotaTypeLabel})

	nodeLoadCVGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scheduler_node_load_cv",
		Help: "Coefficient of variation (population stddev / mean) of per-node load ratio (usage / raw quota) across healthy nodes.",
	}, []string{resourceLabel})

	activeNodesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scheduler_active_nodes",
		Help: "Number of healthy nodes with MvmNum > 0 or non-zero accounted usage.",
	})

	emptyNodesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "scheduler_empty_nodes",
		Help: "Number of healthy nodes with MvmNum == 0 and zero accounted usage.",
	})

	fragmentedCapacityGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scheduler_fragmented_capacity_ratio",
		Help: "Ratio of free capacity stranded on nodes that cannot fit the reference shape (see fragmentedCapacityRatio), by shape.",
	}, []string{shapeLabel})
)

// ObserveScheduleAttempt records one Select call: the attempts counter with a
// classified result/reason, and the duration histogram (observed for both
// success and failure).
func ObserveScheduleAttempt(profile string, err error, d time.Duration) {
	result, reason := metricResultSuccess, attemptReasonNone
	if err != nil {
		result, reason = metricResultError, classifyAttemptError(err)
	}
	schedulerAttempts.WithLabelValues(profile, result, reason).Inc()
	schedulerDuration.WithLabelValues(profile).Observe(d.Seconds())
}

// classifyAttemptError maps the Select failure error code to a small reason
// enum. ret.FromError normalizes non-ret errors to ErrorCode_Unknown, which
// lands in the generic "error" bucket.
func classifyAttemptError(err error) string {
	st, _ := ret.FromError(err)
	switch st.Code() {
	case errorcode.ErrorCode_SelectNodesNoRes:
		return attemptReasonNoNode
	case errorcode.ErrorCode_SelectNodesFailed:
		return attemptReasonPreFilter
	default:
		return attemptReasonError
	}
}

// RecordDecision counts one successful scheduling decision. templateHit marks
// that the selected node already has a local replica of the requested
// template; requests without a template always record false.
func RecordDecision(profile string, templateHit bool) {
	schedulerDecisions.WithLabelValues(profile, strconv.FormatBool(templateHit)).Inc()
}

// RecordReschedule counts one handleCubelet retry/reschedule event,
// categorized from the cubelet/master error code that triggered it.
func RecordReschedule(profile string, code errorcode.ErrorCode) {
	schedulerReschedules.WithLabelValues(profile, rescheduleReason(code)).Inc()
}

// rescheduleReason classifies the error code into a low-cardinality category.
// Precedence matters for codes present in several retry maps (e.g.
// HostDiskNotEnough is both circuit-breaking and loop-retryable): the first
// matching category wins, mirroring how handleCubelet treats the code.
func rescheduleReason(code errorcode.ErrorCode) string {
	switch {
	case code == errorcode.ErrorCode_ReqCubeAPIFailed:
		// errRetry (cubelet RPC failure) always synthesizes this code.
		return rescheduleReasonRPCError
	case errorcode.IsCircutBreakCode(code):
		return rescheduleReasonCircuitBreak
	case errorcode.IsReuseCode(code):
		return rescheduleReasonReuse
	case errorcode.IsLoopRetryCode(code):
		return rescheduleReasonLoopRetry
	case errorcode.IsBackoffRetryCode(code):
		return rescheduleReasonBackoff
	case code == errorcode.ErrorCode_SelectNodesFailed:
		// The scheduling-admission gate rejected the selected node.
		return rescheduleReasonAdmission
	default:
		return rescheduleReasonOther
	}
}

// ObserveSandboxCreate records one finished sandbox create request.
// start/end reuse the createSandboxContext timestamps; an unset or inverted
// end falls back to now so early-return paths still get a sane duration.
func ObserveSandboxCreate(profile string, success bool, start, end time.Time) {
	if start.IsZero() {
		start = time.Now()
	}
	if end.IsZero() || end.Before(start) {
		end = time.Now()
	}
	result := metricResultSuccess
	if !success {
		result = metricResultError
	}
	sandboxCreateAttempts.WithLabelValues(profile, result).Inc()
	sandboxCreateDuration.WithLabelValues(profile, result).Observe(end.Sub(start).Seconds())
}

// observeSelect is the deferred metrics hook of Select. It runs after the
// panic-recovery defer, so it observes the final (nodes, err) pair. A nil
// node with nil error is normalized to a no_node failure: the caller treats
// it as an error anyway.
func observeSelect(selCtx *selctx.SelectorCtx, selected *node.Node, err error, start time.Time) {
	if err == nil && selected == nil {
		err = ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	ObserveScheduleAttempt(DefaultProfile, err, time.Since(start))
	if err != nil {
		return
	}
	RecordDecision(DefaultProfile, templateLocalHit(selCtx, selected))
}

// templateLocalHit reports whether the selected node holds a local replica of
// the requested template. Only checked when the request carries a TemplateID;
// the lookup is an in-memory image-cache read.
func templateLocalHit(selCtx *selctx.SelectorCtx, selected *node.Node) bool {
	if selCtx == nil || selCtx.ReqRes == nil || selCtx.ReqRes.TemplateID == "" || selected == nil {
		return false
	}
	return localcache.GetImageStateByNode(selCtx.ReqRes.TemplateID, selected.ID()) != nil
}

// nodeResourceStat is the projection of node.Node needed by the cluster-level
// gauges. cpu values are in millicores, mem values in MB.
type nodeResourceStat struct {
	instanceType  string
	quotaCpuMilli int64
	quotaMemMB    int64
	cpuUsageMilli int64
	memUsageMB    int64
	mvmNum        int64
}

// nodeCapacityFunc resolves one node's overcommitted schedulable capacity and
// the allocated usage the scheduler accounts for (EffectiveAllocated), in cpu
// millicores / mem MB.
type nodeCapacityFunc func(n nodeResourceStat) (cpuCapMilli, memCapMB, cpuAllocMilli, memAllocMB int64)

// clusterQuota holds summed cluster resources for the quota gauge.
type clusterQuota struct {
	cpuAllocated float64
	cpuCapacity  float64
	memAllocated float64
	memCapacity  float64
}

func sumClusterQuota(nodes []nodeResourceStat, fn nodeCapacityFunc) clusterQuota {
	var q clusterQuota
	for _, n := range nodes {
		cpuCap, memCap, cpuAlloc, memAlloc := fn(n)
		q.cpuCapacity += float64(cpuCap)
		q.memCapacity += float64(memCap)
		q.cpuAllocated += float64(cpuAlloc)
		q.memAllocated += float64(memAlloc)
	}
	return q
}

// nodeLoadCV returns the coefficient of variation of per-node load ratios
// (usage / raw quota), computed over nodes with a positive quota. An empty
// set or zero mean yields 0.
func nodeLoadCV(nodes []nodeResourceStat) (cpuCV, memCV float64) {
	cpuRatios := make([]float64, 0, len(nodes))
	memRatios := make([]float64, 0, len(nodes))
	for _, n := range nodes {
		if n.quotaCpuMilli > 0 {
			cpuRatios = append(cpuRatios, float64(n.cpuUsageMilli)/float64(n.quotaCpuMilli))
		}
		if n.quotaMemMB > 0 {
			memRatios = append(memRatios, float64(n.memUsageMB)/float64(n.quotaMemMB))
		}
	}
	return cvOfRatios(cpuRatios), cvOfRatios(memRatios)
}

// cvOfRatios computes population stddev / mean; the full node set is
// enumerated, so no sample correction is applied.
func cvOfRatios(ratios []float64) float64 {
	if len(ratios) == 0 {
		return 0
	}
	var sum float64
	for _, r := range ratios {
		sum += r
	}
	mean := sum / float64(len(ratios))
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, r := range ratios {
		d := r - mean
		sq += d * d
	}
	return math.Sqrt(sq/float64(len(ratios))) / mean
}

// countActiveEmptyNodes splits nodes into active (running microVMs or any
// accounted usage) and empty.
func countActiveEmptyNodes(nodes []nodeResourceStat) (active, empty int) {
	for _, n := range nodes {
		if n.mvmNum > 0 || n.cpuUsageMilli > 0 || n.memUsageMB > 0 {
			active++
		} else {
			empty++
		}
	}
	return active, empty
}

// fragmentedCapacityRatio measures how much free capacity is stranded on
// nodes that cannot fit the reference shape: per node, free = max(0,
// overcommitted capacity - accounted allocation); a node whose free cpu OR
// free mem is below the shape counts as unfit, and its free resources count
// as fragmented. The result is the mean of the cpu and mem fragmented ratios,
// in [0,1]; a resource whose cluster-wide free total is 0 contributes 0.
//
// The reference shape is MaxMvmCPU x MaxMvmMemory (label shape="max_mvm"),
// i.e. "cannot host even one largest schedulable microVM" — the only shape
// configured cluster-wide, which keeps the label cardinality at 1.
func fragmentedCapacityRatio(nodes []nodeResourceStat, fn nodeCapacityFunc, shapeCpuMilli, shapeMemMB int64) float64 {
	var totalFreeCpu, totalFreeMem, fragFreeCpu, fragFreeMem float64
	for _, n := range nodes {
		cpuCap, memCap, cpuAlloc, memAlloc := fn(n)
		freeCpu := math.Max(0, float64(cpuCap-cpuAlloc))
		freeMem := math.Max(0, float64(memCap-memAlloc))
		totalFreeCpu += freeCpu
		totalFreeMem += freeMem
		if freeCpu < float64(shapeCpuMilli) || freeMem < float64(shapeMemMB) {
			fragFreeCpu += freeCpu
			fragFreeMem += freeMem
		}
	}
	fragCpu, fragMem := 0.0, 0.0
	if totalFreeCpu > 0 {
		fragCpu = fragFreeCpu / totalFreeCpu
	}
	if totalFreeMem > 0 {
		fragMem = fragFreeMem / totalFreeMem
	}
	return (fragCpu + fragMem) / 2
}

var clusterGaugeOnce sync.Once

// startClusterGaugeCollector launches the periodic gauge collection exactly
// once, even if InitScheduler is called repeatedly (e.g. in-process restarts).
func startClusterGaugeCollector(ctx context.Context) {
	clusterGaugeOnce.Do(func() {
		recov.GoWithRecover(func() {
			collectClusterGaugesLoop(ctx)
		})
	})
}

func collectClusterGaugesLoop(ctx context.Context) {
	ticker := time.NewTicker(clusterGaugeCollectInterval)
	defer ticker.Stop()
	for {
		recov.WithRecover(collectClusterGauges, func(panicError interface{}) {
			log.G(ctx).Errorf("collectClusterGauges panic:%v", panicError)
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// collectClusterGauges recomputes all cluster-level gauges from the healthy
// node set in localcache. Unhealthy nodes are excluded (consistent with the
// existing stdev/limit loops); cordoned-but-healthy nodes stay included since
// they still hold capacity and load.
func collectClusterGauges() {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Scheduler == nil {
		return
	}
	nodes := localcache.GetHealthyNodes(-1)
	stats := make([]nodeResourceStat, 0, nodes.Len())
	for _, n := range nodes {
		if n == nil {
			continue
		}
		stats = append(stats, nodeResourceStat{
			instanceType:  n.InstanceType,
			quotaCpuMilli: n.QuotaCpu,
			quotaMemMB:    n.QuotaMem,
			cpuUsageMilli: n.QuotaCpuUsage,
			memUsageMB:    n.QuotaMemUsage,
			mvmNum:        n.MvmNum,
		})
	}
	capFn := func(n nodeResourceStat) (int64, int64, int64, int64) {
		return cfg.Scheduler.EffectiveQuotaCpu(n.instanceType, n.quotaCpuMilli),
			cfg.Scheduler.EffectiveQuotaMem(n.instanceType, n.quotaMemMB),
			cfg.Scheduler.EffectiveAllocated(n.cpuUsageMilli),
			cfg.Scheduler.EffectiveAllocated(n.memUsageMB)
	}

	quota := sumClusterQuota(stats, capFn)
	clusterQuotaGauge.WithLabelValues(resourceLabelCPU, quotaTypeAllocated).Set(quota.cpuAllocated)
	clusterQuotaGauge.WithLabelValues(resourceLabelCPU, quotaTypeCapacity).Set(quota.cpuCapacity)
	clusterQuotaGauge.WithLabelValues(resourceLabelMem, quotaTypeAllocated).Set(quota.memAllocated)
	clusterQuotaGauge.WithLabelValues(resourceLabelMem, quotaTypeCapacity).Set(quota.memCapacity)

	cpuCV, memCV := nodeLoadCV(stats)
	nodeLoadCVGauge.WithLabelValues(resourceLabelCPU).Set(cpuCV)
	nodeLoadCVGauge.WithLabelValues(resourceLabelMem).Set(memCV)

	active, empty := countActiveEmptyNodes(stats)
	activeNodesGauge.Set(float64(active))
	emptyNodesGauge.Set(float64(empty))

	shapeCPU := cfg.Scheduler.MaxMvmCPURes()
	shapeMem := cfg.Scheduler.MaxMvmMemoryRes()
	fragmentedCapacityGauge.WithLabelValues(fragmentShapeMaxMVM).
		Set(fragmentedCapacityRatio(stats, capFn, shapeCPU.MilliValue(), shapeMem.Value()/(1024*1024)))
}

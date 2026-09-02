// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// Bootstrap loads the CubeMaster config from configPath and initializes the
// in-process scheduler pipeline (prefilter + filters + scorers) exactly the
// way the real master does, minus DB/Redis-backed loops: localcache.Init is
// deliberately NOT called so no DB/Redis connections or background sync
// goroutines start; the package-level localcache stores are already usable.
//
// task.InitTask is required: scheduler.InitScheduler spawns a monitorLimit
// goroutine that periodically calls task.SetTaskWorkerConcurrent, which
// nil-panics (recovered, but noisy) when the task package was never init'ed.
//
// Bootstrap is process-global and one-shot: the first call performs the init
// and every later call is a no-op returning the first call's result, so the
// config path is fixed by the first call. Library consumers must not expect
// to re-init with a different config in the same process.
//
// Note for would-be embedders: Bootstrap also forces the process-wide
// CubeLog level to FATAL, installs the config singleton, and leaves the
// config-watcher and scheduler monitor/collect goroutines running on the
// passed context for the life of the process. It is meant for the
// short-lived schedsim CLI and tests, not for embedding in a long-lived
// binary.
func Bootstrap(ctx context.Context, configPath string) error {
	bootOnce.Do(func() {
		bootErr = bootstrap(ctx, configPath)
	})
	return bootErr
}

var (
	bootOnce sync.Once
	bootErr  error
)

func bootstrap(ctx context.Context, configPath string) error {
	if configPath == "" {
		return errors.New("sim: config path is required")
	}
	if err := os.Setenv("CUBE_MASTER_CONFIG_PATH", configPath); err != nil {
		return fmt.Errorf("sim: set CUBE_MASTER_CONFIG_PATH: %w", err)
	}
	if _, err := config.Init(); err != nil {
		return fmt.Errorf("sim: config.Init: %w", err)
	}
	// With ignore_redis_allocation the filters' EffectiveAllocated is 0, so
	// the scheduler never observes the sim's in-process usage pushes and the
	// quota gates never bind — every utilization/balance metric degenerates.
	if sc := config.GetConfig().Scheduler; sc != nil && sc.ShouldIgnoreRedisAllocation() {
		fmt.Fprintln(os.Stderr, "schedsim: WARNING: scheduler.ignore_redis_allocation=true makes the scheduler "+
			"ignore the sim's allocation pushes; quota gates never bind and utilization metrics are meaningless — "+
			"set it to false for simulation")
	}
	// The scheduler core logs per-request at Info/Warn; that would flood the
	// sim output and pollute the Select latency measurement.
	CubeLog.SetLevel(CubeLog.FATAL)
	task.InitTask(ctx, config.GetConfig())
	scheduler.InitScheduler(ctx)
	return nil
}

// Params describes one simulation round.
type Params struct {
	Trace *Trace

	Nodes         int
	NodeCPUMillis int64
	NodeMemMiB    int64
	InstanceType  string

	// TemplatePreload is the fraction of nodes that receive a local replica of
	// each template up front (per template, per node, drawn from the seeded
	// rng). 1.0 = every node has every template.
	TemplatePreload float64

	// AllowNonLocalTemplate relaxes strict template locality. When false
	// (default), requests insist on a node holding a local replica, matching
	// the template_locality filter's strict semantics — which makes
	// template_hit_rate ≡ 1 by construction whenever that filter is enabled.
	// When true, requests may land on nodes without a local replica, modeling
	// the S3 remote_ready cross-node restore path and turning
	// template_hit_rate into a dynamic miss→warm→hit measurement.
	AllowNonLocalTemplate bool

	// Seed drives template preload placement for this round. Round i of a
	// multi-round run uses base seed + i.
	Seed int64
	// RoundID uniquifies injected node IDs so rounds never collide inside the
	// process-wide localcache.
	RoundID int
}

func (p *Params) validate() error {
	if p.Trace == nil || len(p.Trace.Requests) == 0 {
		return errors.New("sim: trace with at least one request is required")
	}
	if p.Nodes <= 0 {
		return errors.New("sim: nodes must be > 0")
	}
	if p.NodeCPUMillis <= 0 || p.NodeMemMiB <= 0 {
		return errors.New("sim: node cpu/mem spec must be > 0")
	}
	if p.InstanceType == "" {
		return errors.New("sim: instance type must not be empty")
	}
	if p.TemplatePreload < 0 || p.TemplatePreload > 1 {
		return fmt.Errorf("sim: template preload ratio %v out of [0,1]", p.TemplatePreload)
	}
	// RunRound is exported and accepts in-memory traces, so the LoadTrace
	// request-field checks must run here too: a negative lifetime would push
	// an expiry ahead of its own create and silently corrupt the round's
	// time-weighted metrics instead of failing.
	if err := p.Trace.validate(); err != nil {
		return fmt.Errorf("sim: invalid trace: %w", err)
	}
	return nil
}

// SummaryKeys lists every key present in a round summary, in stable order.
var SummaryKeys = []string{
	"success_rate",
	"sched_latency_p50_ms",
	"sched_latency_p95_ms",
	"sched_latency_p99_ms",
	"cpu_alloc_rate",
	"mem_alloc_rate",
	"load_cv_cpu",
	"load_cv_mem",
	"jain_cpu",
	"jain_mem",
	"fragmentation_ratio",
	"fragmentation_ratio_mem",
	"herding_top1_share",
	"template_hit_rate",
	"active_nodes_avg",
	"empty_nodes_avg",
	"metric_push_errors",
}

// RoundResult carries one round's seed and its flat summary metrics.
type RoundResult struct {
	Seed    int64              `json:"seed"`
	Summary map[string]float64 `json:"summary"`
}

// nodeState is the simulator's own book on a node; localcache mirrors it via
// UpdateNodeMetricInProcess so subsequent Select calls observe the usage.
type nodeState struct {
	id            string
	quotaCPUMilli int64
	quotaMemMiB   int64
	usedCPUMilli  int64
	usedMemMiB    int64
	running       int64
}

type placement struct {
	nodeID     string
	cpuMillis  int64
	memMiB     int64
	templateID string
}

type eventKind int

const (
	// evExpire sorts before evCreate at equal virtual timestamps so resources
	// freed at T are already available to requests arriving at T.
	evExpire eventKind = iota
	evCreate
)

type event struct {
	timeMs  int64
	kind    eventKind
	reqIdx  int // evCreate: index into Trace.Requests
	placeID int // evExpire: placement id
}

// eventHeap is a min-heap ordered by (timeMs, kind, reqIdx).
type eventHeap []event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].timeMs != h[j].timeMs {
		return h[i].timeMs < h[j].timeMs
	}
	if h[i].kind != h[j].kind {
		return h[i].kind < h[j].kind
	}
	return h[i].reqIdx < h[j].reqIdx
}
func (h eventHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x interface{}) { *h = append(*h, x.(event)) }
func (h *eventHeap) Pop() interface{} {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

type engine struct {
	p Params

	// maxReqCPUMillis is the largest cpu_millis among the trace's FEASIBLE
	// requests — those at least one empty node could admit (see
	// maxFeasibleShape). It is a constant of the trace and node spec,
	// precomputed once so snapshot() stays O(nodes) instead of rescanning
	// every request after every event.
	maxReqCPUMillis int64
	// maxReqMemMiB is the same for mem_mib (memory can bind before CPU under
	// asymmetric overcommit ratios, so fragmentation is tracked per resource).
	maxReqMemMiB int64

	nodeOrder []*nodeState
	nodes     map[string]*nodeState
	replicas  map[string]map[string]bool // templateID -> nodeIDs with a local replica

	// registered records every (templateID, nodeID) pair pushed into
	// localcache so the round can deregister exactly those on cleanup.
	registered [][2]string

	events      eventHeap
	placements  map[int]*placement
	nextPlaceID int

	sampler   TimeWeightedAvg
	latencyMs []float64

	successes          int
	failures           int
	templatedSuccesses int
	templateHits       int
	nodeSuccess        map[string]int
	metricPushErrors   int
}

// RunRound executes one full simulation round: inject nodes, preload template
// replicas, replay the trace on a virtual clock, then withdraw the round's
// nodes/replicas from localcache. The real scheduler core decides placements;
// the wall-clock cost of every Select call is recorded as scheduling latency.
func RunRound(ctx context.Context, p Params) (*RoundResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	e := &engine{
		p:           p,
		nodes:       make(map[string]*nodeState, p.Nodes),
		replicas:    make(map[string]map[string]bool),
		placements:  make(map[int]*placement),
		nodeSuccess: make(map[string]int),
	}
	e.maxReqCPUMillis, e.maxReqMemMiB = e.maxFeasibleShape()
	e.injectNodes()
	defer e.cleanup()
	e.preloadTemplates()
	e.runEvents(ctx)
	return e.result(), nil
}

// effectiveOvercommitRatios returns the cpu/mem overcommit multipliers the
// cpu and mem filters admit on for the sim instance type, defaulting to 1.0
// when the scheduler config is absent or carries a non-positive ratio.
func (e *engine) effectiveOvercommitRatios() (cpuRatio, memRatio float64) {
	cpuRatio, memRatio = 1.0, 1.0
	if sc := config.GetConfig().Scheduler; sc != nil {
		oc := sc.GetEffectiveOvercommitRatio(e.p.InstanceType)
		if oc.CPURatio > 0 {
			cpuRatio = oc.CPURatio
		}
		if oc.MemRatio > 0 {
			memRatio = oc.MemRatio
		}
	}
	return cpuRatio, memRatio
}

// maxFeasibleShape returns the largest per-resource request shape that an
// EMPTY node could still admit, mirroring the filters' strict free > req
// check against the effective (overcommit-aware) capacity of an empty node;
// nodes are homogeneous, so one feasibility test covers the fleet. Requests
// no empty node could ever fit are excluded: they are rejected wherever they
// land, so counting them in the fragmentation shape would peg
// fragmentation_ratio at ~1 for the whole window and measure the
// unplaceable outlier instead of scheduler-induced stranding. When no
// request is feasible the plain trace max is kept (success_rate is 0 anyway,
// and the metric stays defined).
func (e *engine) maxFeasibleShape() (cpuMillis, memMiB int64) {
	cpuRatio, memRatio := e.effectiveOvercommitRatios()
	emptyCPU := float64(e.p.NodeCPUMillis) * cpuRatio
	emptyMem := float64(e.p.NodeMemMiB) * memRatio
	feasible := false
	for i := range e.p.Trace.Requests {
		r := &e.p.Trace.Requests[i]
		if float64(r.CpuMillis) >= emptyCPU || float64(r.MemMiB) >= emptyMem {
			continue
		}
		feasible = true
		if r.CpuMillis > cpuMillis {
			cpuMillis = r.CpuMillis
		}
		if r.MemMiB > memMiB {
			memMiB = r.MemMiB
		}
	}
	if !feasible {
		return e.p.Trace.MaxRequestCpuMillis(), e.p.Trace.MaxRequestMemMiB()
	}
	return cpuMillis, memMiB
}

// ossClusterLabel returns the cluster label injected on sim nodes. Configs may
// map it to the sim instance type via scheduler.instance_type_conf; when the
// mapping is absent (or localcache.Init never ran) enumeration transparently
// falls back to scanning every healthy node in the cache, which is exactly the
// sim node set.
func (p *Params) ossClusterLabel() string {
	return "schedsim-" + p.InstanceType
}

func (e *engine) nodeID(i int) string {
	return fmt.Sprintf("schedsim-r%d-n%05d", e.p.RoundID, i)
}

func (e *engine) injectNodes() {
	now := time.Now()
	e.nodeOrder = make([]*nodeState, 0, e.p.Nodes)
	for i := 0; i < e.p.Nodes; i++ {
		id := e.nodeID(i)
		n := &node.Node{
			Index:           i + 1,
			InsID:           id,
			IP:              fmt.Sprintf("10.250.%d.%d", (i/256)%256, i%256),
			Healthy:         true,
			ReportedReady:   true,
			InstanceType:    e.p.InstanceType,
			ClusterLabel:    e.p.ossClusterLabel(),
			OssClusterLabel: e.p.ossClusterLabel(),
			// QuotaCpu is in millicores and QuotaMem in MiB, matching the
			// units the cpu/mem filters compare against request quantities.
			QuotaCpu:            e.p.NodeCPUMillis,
			QuotaMem:            e.p.NodeMemMiB,
			CpuTotal:            int(e.p.NodeCPUMillis / 1000),
			MemMBTotal:          e.p.NodeMemMiB,
			MetaDataUpdateAt:    now,
			MetricUpdate:        now,
			MetricLocalUpdateAt: now,
		}
		localcache.UpsertNode(n)
		ns := &nodeState{
			id:            id,
			quotaCPUMilli: e.p.NodeCPUMillis,
			quotaMemMiB:   e.p.NodeMemMiB,
		}
		e.nodes[id] = ns
		e.nodeOrder = append(e.nodeOrder, ns)
	}
}

// cleanup withdraws the round's nodes (marked unhealthy, which drops them from
// the schedulable enumeration) and deregisters exactly the template replicas
// this round registered, so rounds cannot leak state into each other even
// though localcache exposes no bulk-clear API.
func (e *engine) cleanup() {
	for _, pair := range e.registered {
		localcache.DeregisterTemplateReplica(pair[0], pair[1])
	}
	for _, ns := range e.nodeOrder {
		localcache.UpsertNode(&node.Node{
			InsID:            ns.id,
			IP:               "",
			Healthy:          false,
			UnhealthyReason:  "schedsim round finished",
			InstanceType:     e.p.InstanceType,
			OssClusterLabel:  e.p.ossClusterLabel(),
			MetaDataUpdateAt: time.Now(),
		})
	}
}

// preloadTemplates registers a local replica of every trace template on a
// seeded-random subset of nodes (TemplatePreload fraction). The draw is the
// only rng-driven input to a round; it is fully determined by Params.Seed.
func (e *engine) preloadTemplates() {
	if e.p.TemplatePreload <= 0 {
		return
	}
	rng := rand.New(rand.NewSource(e.p.Seed))
	for _, tplID := range e.p.Trace.TemplateIDs() {
		for _, ns := range e.nodeOrder {
			if e.p.TemplatePreload < 1 && rng.Float64() >= e.p.TemplatePreload {
				continue
			}
			localcache.RegisterTemplateReplica(tplID, ns.id, 1)
			if e.replicas[tplID] == nil {
				e.replicas[tplID] = make(map[string]bool)
			}
			e.replicas[tplID][ns.id] = true
			e.registered = append(e.registered, [2]string{tplID, ns.id})
		}
	}
}

func (e *engine) runEvents(ctx context.Context) {
	for i := range e.p.Trace.Requests {
		heap.Push(&e.events, event{timeMs: e.p.Trace.Requests[i].ArrivalMs, kind: evCreate, reqIdx: i})
	}
	// Install the empty-cluster snapshot at t=0 so the fully idle prefix
	// [0, firstArrival) is credited in the time-weighted averages instead of
	// being silently dropped; a no-op when the first arrival is at t=0.
	e.sampler.Advance(0, e.snapshot())
	for e.events.Len() > 0 {
		ev := heap.Pop(&e.events).(event)
		switch ev.kind {
		case evCreate:
			e.onCreate(ctx, ev)
		case evExpire:
			e.onExpire(ev)
		}
		// Sample after every event; the sampler credits the previous state for
		// the virtual time it persisted.
		e.sampler.Advance(ev.timeMs, e.snapshot())
	}

	// Close the averaging window at a trace-derived horizon instead of the
	// last event. Rejected requests push no expiry, so an event-derived
	// window would end earlier for configs that reject the trace tail — the
	// time denominator would drift with the scheduler's own success rate and
	// break A/B comparability. max(arrival_ms)+max(lifetime_ms) upper-bounds
	// every admitted placement's expiry, and every admitted placement has
	// expired by then, so the tail credits only the drained (zero-load) state.
	var horizonMs int64
	for _, r := range e.p.Trace.Requests {
		if end := r.ArrivalMs + r.LifetimeMs; end > horizonMs {
			horizonMs = end
		}
	}
	e.sampler.Advance(horizonMs, e.snapshot())
}

func (e *engine) onCreate(ctx context.Context, ev event) {
	req := e.p.Trace.Requests[ev.reqIdx]

	selName := "random"
	if sc := config.GetConfig().Scheduler; sc != nil && sc.LeastSelectName != "" {
		selName = sc.LeastSelectName
	}
	selCtx := selctx.New(selName)
	selCtx.Ctx = ctx
	selCtx.InstanceType = e.p.InstanceType
	selCtx.ReqRes = &selctx.RequestResource{
		Cpu: *resource.NewMilliQuantity(req.CpuMillis, resource.DecimalSI),
		Mem: *resource.NewQuantity(req.MemMiB*1024*1024, resource.BinarySI),
		// Template locality defaults to strict (see Params.AllowNonLocalTemplate):
		// requests insist on a local replica so locality quality is measurable.
		TemplateID:            req.TemplateID,
		AllowNonLocalTemplate: e.p.AllowNonLocalTemplate,
	}

	start := time.Now()
	selected, err := scheduler.Select(selCtx)
	e.latencyMs = append(e.latencyMs, float64(time.Since(start))/float64(time.Millisecond))

	if err != nil || selected == nil {
		e.failures++
		return
	}
	ns, ok := e.nodes[selected.ID()]
	if !ok {
		// The scheduler must only return injected sim nodes; treat anything
		// else as a failure rather than corrupting the books.
		e.failures++
		return
	}
	e.successes++
	ns.usedCPUMilli += req.CpuMillis
	ns.usedMemMiB += req.MemMiB
	ns.running++
	e.pushMetric(ns)

	e.nextPlaceID++
	e.placements[e.nextPlaceID] = &placement{
		nodeID:     ns.id,
		cpuMillis:  req.CpuMillis,
		memMiB:     req.MemMiB,
		templateID: req.TemplateID,
	}
	heap.Push(&e.events, event{
		timeMs:  ev.timeMs + req.LifetimeMs,
		kind:    evExpire,
		reqIdx:  ev.reqIdx,
		placeID: e.nextPlaceID,
	})

	e.nodeSuccess[ns.id]++
	if req.TemplateID != "" {
		e.templatedSuccesses++
		if e.noteTemplatePlacement(req.TemplateID, ns.id) {
			e.templateHits++
		}
	}
}

// noteTemplatePlacement records a successful placement of a templated request
// and reports whether the node already held a local replica (a template hit).
// On a miss it warms the node: a successful placement pulls the template, so
// later requests for it hit the local replica. (With the template_locality
// filter enabled the miss branch is unreachable — placement already required
// the replica — but with it off, template_hit_rate now tracks dynamic
// locality instead of only the static preload policy.)
func (e *engine) noteTemplatePlacement(tplID, nodeID string) bool {
	if e.replicas[tplID][nodeID] {
		return true
	}
	localcache.RegisterTemplateReplica(tplID, nodeID, 1)
	if e.replicas[tplID] == nil {
		e.replicas[tplID] = make(map[string]bool)
	}
	e.replicas[tplID][nodeID] = true
	e.registered = append(e.registered, [2]string{tplID, nodeID})
	return false
}

func (e *engine) onExpire(ev event) {
	pl, ok := e.placements[ev.placeID]
	if !ok {
		return
	}
	delete(e.placements, ev.placeID)
	ns, ok := e.nodes[pl.nodeID]
	if !ok {
		return
	}
	ns.usedCPUMilli -= pl.cpuMillis
	ns.usedMemMiB -= pl.memMiB
	ns.running--
	e.pushMetric(ns)
}

// pushMetric mirrors the simulator's book into localcache with a fresh
// real-clock timestamp, so the scheduler observes current usage and the
// prefilter metric-freshness check never trips mid-round.
func (e *engine) pushMetric(ns *nodeState) {
	err := localcache.UpdateNodeMetricInProcess(&localcache.NodeMetric{
		NodeID:        ns.id,
		MetricTime:    time.Now(),
		HasAllocated:  true,
		MilliCPUUsage: ns.usedCPUMilli,
		MemoryMBUsage: ns.usedMemMiB,
		MvmNum:        ns.running,
	})
	if err != nil {
		e.metricPushErrors++
	}
}

// snapshot computes the instantaneous cluster state from the simulator's own
// books (not from localcache clones).
func (e *engine) snapshot() Snapshot {
	n := len(e.nodeOrder)
	if n == 0 {
		return Snapshot{}
	}
	cpuRates := make([]float64, 0, n)
	memRates := make([]float64, 0, n)
	freeCPU := make([]float64, 0, n)
	freeMem := make([]float64, 0, n)
	var usedCPU, usedMem, quotaCPU, quotaMem float64
	var active float64

	// Fragmentation is computed against the same effective (overcommit-aware)
	// free CPU/mem the cpu and mem filters admit on — per resource, because
	// either one can bind first depending on the overcommit ratios and the
	// trace shape (e.g. mem_ratio < cpu_ratio with small shapes strands CPU).
	// The shape is the largest FEASIBLE request (maxFeasibleShape): requests
	// no empty node could admit are rejected wherever they land, so they must
	// not mark every node unfit and peg the ratio at ~1.
	cpuRatio, memRatio := e.effectiveOvercommitRatios()

	for _, ns := range e.nodeOrder {
		usedCPU += float64(ns.usedCPUMilli)
		usedMem += float64(ns.usedMemMiB)
		quotaCPU += float64(ns.quotaCPUMilli)
		quotaMem += float64(ns.quotaMemMiB)
		cpuRates = append(cpuRates, float64(ns.usedCPUMilli)/float64(ns.quotaCPUMilli))
		memRates = append(memRates, float64(ns.usedMemMiB)/float64(ns.quotaMemMiB))
		freeC := float64(ns.quotaCPUMilli)*cpuRatio - float64(ns.usedCPUMilli)
		if freeC < 0 {
			freeC = 0
		}
		freeCPU = append(freeCPU, freeC)
		freeM := float64(ns.quotaMemMiB)*memRatio - float64(ns.usedMemMiB)
		if freeM < 0 {
			freeM = 0
		}
		freeMem = append(freeMem, freeM)
		if ns.running > 0 {
			active++
		}
	}

	s := Snapshot{
		LoadCVCPU:             CoefficientOfVariation(cpuRates),
		LoadCVMem:             CoefficientOfVariation(memRates),
		JainCPU:               JainIndex(cpuRates),
		JainMem:               JainIndex(memRates),
		FragmentationRatio:    FragmentationRatio(freeCPU, float64(e.maxReqCPUMillis)),
		FragmentationRatioMem: FragmentationRatio(freeMem, float64(e.maxReqMemMiB)),
		ActiveNodes:           active,
		EmptyNodes:            float64(n) - active,
	}
	if quotaCPU > 0 {
		s.CPUAllocRate = usedCPU / quotaCPU
	}
	if quotaMem > 0 {
		s.MemAllocRate = usedMem / quotaMem
	}
	return s
}

func (e *engine) result() *RoundResult {
	mean := e.sampler.Mean()
	total := len(e.p.Trace.Requests)

	var top1 int
	for _, c := range e.nodeSuccess {
		if c > top1 {
			top1 = c
		}
	}

	summary := map[string]float64{
		"success_rate":            ratio(int64(e.successes), int64(total)),
		"sched_latency_p50_ms":    Percentile(e.latencyMs, 50),
		"sched_latency_p95_ms":    Percentile(e.latencyMs, 95),
		"sched_latency_p99_ms":    Percentile(e.latencyMs, 99),
		"cpu_alloc_rate":          mean.CPUAllocRate,
		"mem_alloc_rate":          mean.MemAllocRate,
		"load_cv_cpu":             mean.LoadCVCPU,
		"load_cv_mem":             mean.LoadCVMem,
		"jain_cpu":                mean.JainCPU,
		"jain_mem":                mean.JainMem,
		"fragmentation_ratio":     mean.FragmentationRatio,
		"fragmentation_ratio_mem": mean.FragmentationRatioMem,
		"herding_top1_share":      ratio(int64(top1), int64(e.successes)),
		"template_hit_rate":       ratio(int64(e.templateHits), int64(e.templatedSuccesses)),
		"active_nodes_avg":        mean.ActiveNodes,
		"empty_nodes_avg":         mean.EmptyNodes,
		// Surfaced (not just counted) so a desync between the sim's book and
		// the localcache state the scheduler admits on is visible in the
		// report instead of silently invalidating the round.
		"metric_push_errors": float64(e.metricPushErrors),
	}
	return &RoundResult{Seed: e.p.Seed, Summary: summary}
}

func ratio(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// bootstrapped guards the one-time process bootstrap: config.Init and
// scheduler.InitScheduler install process-wide state, so all engine tests
// share a single init. The config under test is testdata/sim.test.yaml — a
// frozen copy of the shipped example, so user edits to
// cmd/schedsim/example.sim.yaml cannot break these tests; the example's
// loadability is covered separately by TestShippedExampleConfigParses. The
// error is captured (not t.Fatalf inside Once.Do — a Goexit still marks the
// Once done) so every test fails with the root cause instead of cascading
// into half-initialized downstream failures.
var (
	bootstrapped sync.Once
	bootstrapErr error
)

func bootstrapOnce(t *testing.T) {
	t.Helper()
	bootstrapped.Do(func() {
		cfgPath, err := filepath.Abs("testdata/sim.test.yaml")
		if err != nil {
			bootstrapErr = fmt.Errorf("resolve test config: %w", err)
			return
		}
		bootstrapErr = Bootstrap(context.Background(), cfgPath)
	})
	if bootstrapErr != nil {
		t.Fatalf("bootstrap: %v", bootstrapErr)
	}
}

// TestShippedExampleConfigParses keeps the shipped, user-editable example
// loadable without coupling the engine tests to it: parse-only into the
// config struct config.Init uses (Bootstrap/config.Init are process-global
// one-shot and already consumed by bootstrapOnce, so they must not run here).
func TestShippedExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../../cmd/schedsim/example.sim.yaml")
	if err != nil {
		t.Fatalf("read shipped example: %v", err)
	}
	cfg := &config.Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		t.Fatalf("shipped example does not parse into config.Config: %v", err)
	}
	if cfg.Scheduler == nil {
		t.Fatalf("shipped example lost its scheduler section")
	}
}

func approx(t *testing.T, name string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Fatalf("%s: got %v, want %v ±%v", name, got, want, eps)
	}
}

func mkTrace(n int, arrivalStepMs, lifetimeMs, cpuMillis, memMiB int64, tpl string) *Trace {
	tr := &Trace{
		Workload: "test",
		Templates: []TraceTemplate{
			{TemplateID: tpl, Weight: 1, CpuMillis: cpuMillis, MemMiB: memMiB},
		},
	}
	for i := 0; i < n; i++ {
		tr.Requests = append(tr.Requests, TraceRequest{
			Seq:        i,
			ArrivalMs:  int64(i) * arrivalStepMs,
			TemplateID: tpl,
			CpuMillis:  cpuMillis,
			MemMiB:     memMiB,
			LifetimeMs: lifetimeMs,
		})
	}
	return tr
}

// TestRunRoundSingleNodeConcentrates: with one node every placement lands on
// it, so balance metrics must report perfect concentration-invariant values
// (cv=0, jain=1, top1=1) and the time-averaged alloc rate must match the
// hand-computed integral of the request lifetimes.
func TestRunRoundSingleNodeConcentrates(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:           mkTrace(20, 1000, 60000, 1000, 2048, "tpl-a"),
		Nodes:           1,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            42,
		RoundID:         0,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 1, 1e-9)
	approx(t, "template_hit_rate", s["template_hit_rate"], 1, 1e-9)
	approx(t, "load_cv_cpu", s["load_cv_cpu"], 0, 1e-9)
	approx(t, "load_cv_mem", s["load_cv_mem"], 0, 1e-9)
	approx(t, "jain_cpu", s["jain_cpu"], 1, 1e-9)
	approx(t, "jain_mem", s["jain_mem"], 1, 1e-9)
	approx(t, "herding_top1_share", s["herding_top1_share"], 1, 1e-9)
	approx(t, "active_nodes_avg", s["active_nodes_avg"], 1, 1e-9)
	approx(t, "empty_nodes_avg", s["empty_nodes_avg"], 0, 1e-9)
	approx(t, "metric_push_errors", s["metric_push_errors"], 0, 1e-9)
	approx(t, "fragmentation_ratio", s["fragmentation_ratio"], 0, 1e-9)
	// Mem side: effective free mem (65536×2 − 40960 max used) never drops to
	// the 2048MiB max shape either, so both resource ratios stay 0.
	approx(t, "fragmentation_ratio_mem", s["fragmentation_ratio_mem"], 0, 1e-9)
	// Hand-computed over the virtual span [0,79s]: ramp-up requests*s=190,
	// plateau 20*41=820, ramp-down 190 -> cpu rate integral 1200/64*1000 ms
	// over 79000 ms.
	approx(t, "cpu_alloc_rate", s["cpu_alloc_rate"], 18750.0/79000.0, 1e-6)
	approx(t, "mem_alloc_rate", s["mem_alloc_rate"], 2*18750.0/79000.0, 1e-6)
	for _, k := range []string{"sched_latency_p50_ms", "sched_latency_p95_ms", "sched_latency_p99_ms"} {
		if v, ok := s[k]; !ok || v < 0 {
			t.Fatalf("%s missing or negative: %v", k, s[k])
		}
	}
}

// TestRunRoundSpreadsAcrossNodes: with the least-loaded top-1 policy from
// example.sim.yaml, 8 identical requests over 4 nodes must distribute 2-2-2-2
// regardless of which physical nodes ties break to.
func TestRunRoundSpreadsAcrossNodes(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:           mkTrace(8, 1000, 60000, 1000, 2048, "tpl-b"),
		Nodes:           4,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            7,
		RoundID:         1,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 1, 1e-9)
	approx(t, "template_hit_rate", s["template_hit_rate"], 1, 1e-9)
	// Perfectly even 2-2-2-2 distribution: the busiest node took exactly 1/4.
	approx(t, "herding_top1_share", s["herding_top1_share"], 0.25, 1e-9)
	// Hand-computed: active nodes integrate to 256 node-seconds over the 67s
	// virtual span (ramp 1+2+3, plateau 4*61, drain 3+2+1).
	approx(t, "active_nodes_avg", s["active_nodes_avg"], 256.0/67.0, 1e-6)
	approx(t, "empty_nodes_avg", s["empty_nodes_avg"], 4-256.0/67.0, 1e-6)
	// Balance is perfect for the 53s plateau and imperfect only during the
	// 7s ramp / 7s drain.
	if got := s["jain_cpu"]; got < 0.9 || got > 1 {
		t.Fatalf("jain_cpu out of expected range: %v", got)
	}
	if got := s["load_cv_cpu"]; got <= 0.02 || got >= 0.25 {
		t.Fatalf("load_cv_cpu out of expected range: %v", got)
	}
}

// TestRunRoundTemplateMissFails: with template_locality enabled and no
// preloaded replica, every template-bound request must be rejected (the
// template skip-backoff path returns the filter error directly).
func TestRunRoundTemplateMissFails(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:           mkTrace(4, 1000, 60000, 1000, 2048, "tpl-never-preloaded"),
		Nodes:           2,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 0,
		Seed:            1,
		RoundID:         2,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 0, 1e-9)
	approx(t, "template_hit_rate", s["template_hit_rate"], 0, 1e-9)
	approx(t, "herding_top1_share", s["herding_top1_share"], 0, 1e-9)
	approx(t, "cpu_alloc_rate", s["cpu_alloc_rate"], 0, 1e-9)
	approx(t, "active_nodes_avg", s["active_nodes_avg"], 0, 1e-9)
	approx(t, "empty_nodes_avg", s["empty_nodes_avg"], 2, 1e-9)
}

// TestRunRoundAllowNonLocalTemplate: with the locality constraint relaxed and
// no preloaded replica, requests still succeed (the S3 remote_ready restore
// path); each node's first placement is a miss that warms it, so with a 2-2
// spread over 2 nodes exactly half of the 4 templated placements hit.
func TestRunRoundAllowNonLocalTemplate(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:                 mkTrace(4, 1000, 60000, 1000, 2048, "tpl-remote"),
		Nodes:                 2,
		NodeCPUMillis:         64000,
		NodeMemMiB:            65536,
		InstanceType:          "sim",
		TemplatePreload:       0,
		AllowNonLocalTemplate: true,
		Seed:                  3,
		RoundID:               3,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 1, 1e-9)
	approx(t, "template_hit_rate", s["template_hit_rate"], 0.5, 1e-9)
	approx(t, "metric_push_errors", s["metric_push_errors"], 0, 1e-9)
}

// TestRunRoundSummaryCoversSummaryKeys pins the report contract: a round
// summary carries every SummaryKeys entry (so MeanSummary never silently
// drops a metric), and the aggregated summary has exactly that key set.
func TestRunRoundSummaryCoversSummaryKeys(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:           mkTrace(4, 1000, 60000, 1000, 2048, "tpl-keys"),
		Nodes:           1,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            5,
		RoundID:         4,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	for _, k := range SummaryKeys {
		if _, ok := rr.Summary[k]; !ok {
			t.Fatalf("round summary missing SummaryKeys entry %q", k)
		}
	}
	mean := MeanSummary([]*RoundResult{rr})
	if len(mean) != len(SummaryKeys) {
		t.Fatalf("MeanSummary emitted %d keys, want %d", len(mean), len(SummaryKeys))
	}
	for _, k := range SummaryKeys {
		if mean[k] != rr.Summary[k] {
			t.Fatalf("MeanSummary(%q) = %v over a single round, want %v", k, mean[k], rr.Summary[k])
		}
	}
}

// TestNoteTemplatePlacementWarmsUp: the first placement of a template on a
// node is a miss that warms the node; later placements hit, and every unique
// (template, node) pair is registered exactly once for cleanup.
func TestNoteTemplatePlacementWarmsUp(t *testing.T) {
	e := &engine{replicas: make(map[string]map[string]bool)}

	if hit := e.noteTemplatePlacement("tpl-ut", "sim-node-ut-1"); hit {
		t.Fatalf("first placement on a node must be a miss")
	}
	if hit := e.noteTemplatePlacement("tpl-ut", "sim-node-ut-1"); !hit {
		t.Fatalf("placement after warm-up must hit")
	}
	if hit := e.noteTemplatePlacement("tpl-ut", "sim-node-ut-2"); hit {
		t.Fatalf("first placement on another node must be a miss")
	}
	if len(e.registered) != 2 {
		t.Fatalf("registered pairs = %d, want 2 (one per unique pair)", len(e.registered))
	}
	e.cleanup() // deregister exactly what was registered
}

// TestRunRoundAveragingWindowIsTraceDerived: the time-averaging window must
// close at the trace-derived horizon max(arrival_ms)+max(lifetime_ms), not at
// the last event — a rejected request pushes no expiry, so an event-derived
// window would shrink with the scheduler's own success rate and break A/B
// comparability. Here request #1 (200000 millicores > the 192000 effective
// free = 64000×3 cpu_ratio) is rejected; the admitted request #0 holds
// 1000/64000 of the node over [0,10s) and the window runs to 15s, diluting
// the averages with the drained 5s tail.
func TestRunRoundAveragingWindowIsTraceDerived(t *testing.T) {
	bootstrapOnce(t)
	tr := &Trace{
		Workload: "test",
		Templates: []TraceTemplate{
			{TemplateID: "tpl-win", Weight: 1, CpuMillis: 1000, MemMiB: 2048},
		},
		Requests: []TraceRequest{
			{Seq: 0, ArrivalMs: 0, TemplateID: "tpl-win", CpuMillis: 1000, MemMiB: 2048, LifetimeMs: 10000},
			{Seq: 1, ArrivalMs: 5000, TemplateID: "tpl-win", CpuMillis: 200000, MemMiB: 2048, LifetimeMs: 10000},
		},
	}
	rr, err := RunRound(context.Background(), Params{
		Trace:           tr,
		Nodes:           1,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            11,
		RoundID:         5,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 0.5, 1e-9)
	approx(t, "cpu_alloc_rate", s["cpu_alloc_rate"], (1000.0/64000.0)*(10000.0/15000.0), 1e-9)
	approx(t, "active_nodes_avg", s["active_nodes_avg"], 10000.0/15000.0, 1e-9)
}

// TestRunRoundFragmentationExcludesInfeasibleShape: the fragmentation shape is
// the largest FEASIBLE request. Request #1 (200000 millicores > the 192000
// effective free of an empty node = 64000×3 cpu_ratio) can never be admitted,
// so it must not peg fragmentation_ratio at ~1; with the shape reduced to the
// feasible 1000 millicores every node's free CPU stays above it and the ratio
// is exactly 0 over the whole window. (Counted over the whole trace instead,
// this same run reports fragmentation_ratio == 1 whenever any CPU is free.)
func TestRunRoundFragmentationExcludesInfeasibleShape(t *testing.T) {
	bootstrapOnce(t)
	tr := &Trace{
		Workload: "test",
		Templates: []TraceTemplate{
			{TemplateID: "tpl-frag", Weight: 1, CpuMillis: 1000, MemMiB: 2048},
		},
		Requests: []TraceRequest{
			{Seq: 0, ArrivalMs: 0, TemplateID: "tpl-frag", CpuMillis: 1000, MemMiB: 2048, LifetimeMs: 10000},
			{Seq: 1, ArrivalMs: 5000, TemplateID: "tpl-frag", CpuMillis: 200000, MemMiB: 2048, LifetimeMs: 10000},
		},
	}
	rr, err := RunRound(context.Background(), Params{
		Trace:           tr,
		Nodes:           1,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            13,
		RoundID:         6,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 0.5, 1e-9)
	approx(t, "fragmentation_ratio", s["fragmentation_ratio"], 0, 1e-9)
	approx(t, "fragmentation_ratio_mem", s["fragmentation_ratio_mem"], 0, 1e-9)
}

// TestMaxFeasibleShape pins the shape selection: only requests an empty node
// could admit (strict free > req on both resources against the
// overcommit-aware empty-node capacity) count toward the max; when nothing is
// feasible the plain trace max is the fallback. The test config carries
// cpu_ratio 3.0 / mem_ratio 2.0, so an empty 64000-milli/65536-MiB node
// admits shapes strictly below 192000 millicores and 131072 MiB.
func TestMaxFeasibleShape(t *testing.T) {
	bootstrapOnce(t)
	newEngine := func(reqs ...TraceRequest) *engine {
		return &engine{
			p: Params{
				Trace:         &Trace{Requests: reqs},
				NodeCPUMillis: 64000,
				NodeMemMiB:    65536,
				InstanceType:  "sim",
			},
		}
	}

	t.Run("infeasible outliers excluded per resource", func(t *testing.T) {
		cpu, mem := newEngine(
			TraceRequest{CpuMillis: 1000, MemMiB: 2048},
			TraceRequest{CpuMillis: 200000, MemMiB: 2048}, // cpu-infeasible
			TraceRequest{CpuMillis: 500, MemMiB: 200000},  // mem-infeasible
		).maxFeasibleShape()
		if cpu != 1000 || mem != 2048 {
			t.Fatalf("got (%d, %d), want (1000, 2048)", cpu, mem)
		}
	})

	t.Run("boundary is strict like the filters", func(t *testing.T) {
		cpu, mem := newEngine(
			TraceRequest{CpuMillis: 192000, MemMiB: 2048}, // cpu == empty effective free: not admissible
			TraceRequest{CpuMillis: 1000, MemMiB: 131072}, // mem == empty effective free: not admissible
			TraceRequest{CpuMillis: 3000, MemMiB: 4096},
		).maxFeasibleShape()
		if cpu != 3000 || mem != 4096 {
			t.Fatalf("got (%d, %d), want (3000, 4096)", cpu, mem)
		}
	})

	t.Run("no feasible request falls back to trace max", func(t *testing.T) {
		cpu, mem := newEngine(
			TraceRequest{CpuMillis: 200000, MemMiB: 2048},
			TraceRequest{CpuMillis: 250000, MemMiB: 200000},
		).maxFeasibleShape()
		if cpu != 250000 || mem != 200000 {
			t.Fatalf("got (%d, %d), want trace max (250000, 200000)", cpu, mem)
		}
	})
}

// TestRunRoundRejectsMalformedTrace: RunRound is exported and accepts
// in-memory traces, so the LoadTrace request-field checks must also fire
// through Params.validate — a negative lifetime would otherwise push an
// expiry ahead of its own create and silently corrupt the round's
// time-weighted metrics.
func TestRunRoundRejectsMalformedTrace(t *testing.T) {
	bootstrapOnce(t)
	base := func() Params {
		return Params{
			Trace:           mkTrace(2, 1000, 60000, 1000, 2048, "tpl-bad"),
			Nodes:           1,
			NodeCPUMillis:   64000,
			NodeMemMiB:      65536,
			InstanceType:    "sim",
			TemplatePreload: 1.0,
			Seed:            1,
			RoundID:         7,
		}
	}

	cases := map[string]func(tr *Trace){
		"negative lifetime": func(tr *Trace) { tr.Requests[0].LifetimeMs = -1 },
		"zero cpu":          func(tr *Trace) { tr.Requests[0].CpuMillis = 0 },
		"zero mem":          func(tr *Trace) { tr.Requests[0].MemMiB = 0 },
		"negative arrival":  func(tr *Trace) { tr.Requests[0].ArrivalMs = -1 },
		"unsorted arrival": func(tr *Trace) {
			tr.Requests[0].ArrivalMs = 1000
			tr.Requests[1].ArrivalMs = 500
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base()
			mutate(p.Trace)
			if _, err := RunRound(context.Background(), p); err == nil {
				t.Fatalf("RunRound accepted a malformed trace (%s)", name)
			}
		})
	}
}

// TestRunRoundZeroLifetime: zero-lifetime requests create and expire at the
// same timestamp, exercising the evExpire-before-evCreate tie-break. They are
// valid, must all schedule, and integrate zero occupancy.
func TestRunRoundZeroLifetime(t *testing.T) {
	bootstrapOnce(t)
	rr, err := RunRound(context.Background(), Params{
		Trace:           mkTrace(5, 1000, 0, 1000, 2048, "tpl-zl"),
		Nodes:           2,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            3,
		RoundID:         8,
	})
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	s := rr.Summary
	approx(t, "success_rate", s["success_rate"], 1, 1e-9)
	approx(t, "template_hit_rate", s["template_hit_rate"], 1, 1e-9)
	// Zero-duration bookings integrate no usage over the [0, max arrival)
	// window.
	approx(t, "cpu_alloc_rate", s["cpu_alloc_rate"], 0, 1e-9)
	approx(t, "mem_alloc_rate", s["mem_alloc_rate"], 0, 1e-9)
	approx(t, "active_nodes_avg", s["active_nodes_avg"], 0, 1e-9)
}

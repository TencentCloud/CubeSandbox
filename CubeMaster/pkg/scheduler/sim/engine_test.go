// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
)

// bootstrapped guards the one-time process bootstrap: config.Init and
// scheduler.InitScheduler install process-wide state, so all engine tests
// share a single init. The config under test is the shipped example, which
// doubles as a check that example.sim.yaml stays loadable.
var bootstrapped sync.Once

func bootstrapOnce(t *testing.T) {
	t.Helper()
	bootstrapped.Do(func() {
		cfgPath, err := filepath.Abs("../../../cmd/schedsim/example.sim.yaml")
		if err != nil {
			t.Fatalf("resolve example config: %v", err)
		}
		if err := Bootstrap(context.Background(), cfgPath); err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
	})
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
	approx(t, "fragmentation_ratio", s["fragmentation_ratio"], 0, 1e-9)
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

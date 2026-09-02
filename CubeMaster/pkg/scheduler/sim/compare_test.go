// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"strings"
	"testing"
	"time"
)

// compareFixture builds a two-variant fixture: baseline "legacy" with two
// rounds and candidate "spread" with two rounds, with hand-computable values.
func compareFixture() []VariantReport {
	mk := func(seed int64, summary map[string]float64) *RoundResult {
		return &RoundResult{Seed: seed, Summary: summary}
	}
	baseSummary := map[string]float64{
		"success_rate": 0.90, "load_cv_cpu": 0.40, "sched_latency_p50_ms": 2.0,
		"active_nodes_avg": 10, "cpu_alloc_rate": 0.5,
	}
	candSummary := map[string]float64{
		"success_rate": 0.99, "load_cv_cpu": 0.20, "sched_latency_p50_ms": 4.0,
		"active_nodes_avg": 10, "cpu_alloc_rate": 0.5,
	}
	return []VariantReport{
		{
			Name:       "legacy",
			ConfigPath: "legacy.sim.yaml",
			Report: &Report{
				Config:  ReportConfig{Trace: "t.json", Workload: "burst", Requests: 10, Nodes: 4, NodeCPUMillis: 4000, NodeMemMiB: 4096, InstanceType: "sim", Seed: 42, Rounds: 2},
				Summary: baseSummary,
				Rounds:  []*RoundResult{mk(42, baseSummary), mk(43, baseSummary)},
			},
		},
		{
			Name:       "spread",
			ConfigPath: "spread.sim.yaml",
			Report: &Report{
				Config:  ReportConfig{Trace: "t.json", Workload: "burst", Requests: 10, Nodes: 4, NodeCPUMillis: 4000, NodeMemMiB: 4096, InstanceType: "sim", Seed: 42, Rounds: 2},
				Summary: candSummary,
				Rounds:  []*RoundResult{mk(42, candSummary), mk(43, candSummary)},
			},
		},
	}
}

func TestRenderCompare(t *testing.T) {
	out, err := RenderCompare(compareFixture(), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("RenderCompare: %v", err)
	}

	mustContain := []string{
		"# schedsim A/B Comparison Report",
		"| baseline | legacy | `legacy.sim.yaml` |",
		"| candidate | spread | `spread.sim.yaml` |",
		// identical rounds render exactly 0 stddev, not an epsilon residue
		"0.9 ± 0", "0.99 ± 0",
		// success_rate 0.9 -> 0.99 = +10.0%
		"+10.0%",
		// load_cv_cpu 0.4 -> 0.2 = -50.0%
		"-50.0%",
		// sched_latency_p50_ms 2 -> 4 = +100.0%
		"+100.0%",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}

	// Verdict classification: success_rate (higher-better, +10%) and
	// load_cv_cpu (lower-better, -50%) improve; latency (lower-better, +100%)
	// regresses; identical cpu_alloc_rate has no verdict and directionless
	// active_nodes_avg never concludes even with a delta.
	candIdx := strings.Index(out, "### spread vs legacy")
	if candIdx < 0 {
		t.Fatalf("missing candidate conclusion section:\n%s", out)
	}
	section := out[candIdx:]
	impIdx := strings.Index(section, "Improved:\n")
	regIdx := strings.Index(section, "Regressed:\n")
	if impIdx < 0 || regIdx < 0 || regIdx < impIdx {
		t.Fatalf("malformed conclusion section:\n%s", section)
	}
	improved := section[impIdx:regIdx]
	for _, want := range []string{"success_rate", "load_cv_cpu"} {
		if !strings.Contains(improved, want) {
			t.Fatalf("improved list missing %s:\n%s", want, improved)
		}
	}
	if !strings.Contains(section[regIdx:], "sched_latency_p50_ms") {
		t.Fatalf("regressed list missing latency:\n%s", section[regIdx:])
	}
	if strings.Contains(section, "active_nodes_avg") || strings.Contains(section, "cpu_alloc_rate") {
		t.Fatalf("directionless metrics must not conclude:\n%s", section)
	}
}

func TestRenderCompareSingleSampleNoStddev(t *testing.T) {
	variants := compareFixture()
	// Drop rounds: the top-level summary becomes the single sample.
	for i := range variants {
		variants[i].Report.Rounds = nil
	}
	out, err := RenderCompare(variants, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("RenderCompare: %v", err)
	}
	if strings.Contains(out, "0.9 ±") || strings.Contains(out, "0.99 ±") {
		t.Fatalf("single-sample report must not render stddev in cells:\n%s", out)
	}
	if !strings.Contains(out, "+10.0%") {
		t.Fatalf("single-sample delta missing:\n%s", out)
	}
}

func TestRenderCompareValidation(t *testing.T) {
	if _, err := RenderCompare(nil, time.Unix(0, 0)); err == nil {
		t.Fatal("expected error for empty variants")
	}
	variants := compareFixture()
	variants[1].Report = nil
	if _, err := RenderCompare(variants, time.Unix(0, 0)); err == nil {
		t.Fatal("expected error for nil report")
	}
}

func TestAggregateVariantMissingKeys(t *testing.T) {
	v := &VariantReport{Report: &Report{
		Rounds: []*RoundResult{
			{Seed: 1, Summary: map[string]float64{"success_rate": 0.5}},
			{Seed: 2, Summary: map[string]float64{"success_rate": 1.0}},
		},
	}}
	agg := aggregateVariant(v)
	st, ok := agg["success_rate"]
	if !ok || st.n != 2 {
		t.Fatalf("success_rate stats = %+v, ok=%v", st, ok)
	}
	if st.mean != 0.75 || st.sd != 0.3535533905932738 {
		t.Fatalf("success_rate mean/sd = %v/%v, want 0.75/0.35355...", st.mean, st.sd)
	}
	// Keys absent from every round are simply not present.
	if _, ok := agg["load_cv_cpu"]; ok {
		t.Fatal("load_cv_cpu should be absent")
	}
}

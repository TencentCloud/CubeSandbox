// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPerfModeMatchesQualityMode runs the same deterministic scenario through
// the opaque scheduler.Select (quality) and the sim-side staged driver
// (performance) and requires identical placement-quality summaries. The
// scenario is tie-invariant (8 identical requests over 4 nodes distribute
// 2-2-2-2 under least-loaded scoring no matter how ties break), so every
// placement-derived metric must match exactly; only the wall-clock latency
// keys are allowed to differ.
func TestPerfModeMatchesQualityMode(t *testing.T) {
	bootstrapOnce(t)

	base := Params{
		Trace:           mkTrace(8, 1000, 60000, 1000, 2048, "tpl-perf-eq"),
		Nodes:           4,
		NodeCPUMillis:   64000,
		NodeMemMiB:      65536,
		InstanceType:    "sim",
		TemplatePreload: 1.0,
		Seed:            7,
		RoundID:         40,
	}
	quality, err := RunRound(context.Background(), base)
	if err != nil {
		t.Fatalf("quality RunRound: %v", err)
	}
	perfParams := base
	perfParams.Perf = true
	perfParams.RoundID = 41
	perf, err := RunRound(context.Background(), perfParams)
	if err != nil {
		t.Fatalf("perf RunRound: %v", err)
	}
	if perf.Perf == nil {
		t.Fatalf("perf round must carry a Perf summary")
	}

	for _, k := range SummaryKeys {
		switch k {
		case "sched_latency_p50_ms", "sched_latency_p95_ms", "sched_latency_p99_ms":
			continue // wall-clock values legitimately differ between runs
		}
		approx(t, "quality-vs-perf "+k, perf.Summary[k], quality.Summary[k], 1e-9)
	}

	// Perf plumbing sanity: every request produced one sample per stage, and
	// the staged total is a real wall-clock duration.
	for _, stage := range PerfStageKeys {
		st := perf.Perf.Stages[stage]
		if st == nil {
			t.Fatalf("perf stage %q missing", stage)
		}
		if st.Count != 8 {
			t.Fatalf("stage %q count = %d, want 8", stage, st.Count)
		}
		if st.P99Ms < st.P50Ms {
			t.Fatalf("stage %q p99 < p50 (%v < %v)", stage, st.P99Ms, st.P50Ms)
		}
	}
	if perf.Perf.WallSeconds <= 0 || perf.Perf.ThroughputRPS <= 0 {
		t.Fatalf("perf throughput not recorded: %+v", perf.Perf)
	}

	// Process overhead is attached in perf mode; the Go heap gauges hold on
	// every platform, procfs-derived values on Linux.
	oh := perf.Perf.Overhead
	if oh == nil {
		t.Fatalf("perf round must carry process overhead")
	}
	if oh.HeapAllocMiB <= 0 || oh.HeapSysMiB < oh.HeapAllocMiB {
		t.Fatalf("implausible heap gauges: %+v", oh)
	}
	if runtime.GOOS == "linux" {
		if oh.PeakRSSMiB <= 0 {
			t.Fatalf("linux run must report peak RSS: %+v", oh)
		}
		if oh.CPUSeconds < 0 || oh.AvgCPUCores < 0 {
			t.Fatalf("cpu overhead must be non-negative: %+v", oh)
		}
	}
}

// TestPerfSummaryAggregation hand-checks the percentile and cross-round
// aggregation math.
func TestPerfSummaryAggregation(t *testing.T) {
	s := newStageSamples()
	for i := 1; i <= 100; i++ {
		s.add(stageTimes{
			prefilter: float64(i),
			guards:    1,
			filter:    2,
			score:     3,
			pick:      0.5,
			total:     float64(i) + 6.5,
		})
	}
	perf := s.summarize(0, 0)
	approx(t, "prefilter p50", perf.Stages["prefilter"].P50Ms, 50, 1e-9)
	approx(t, "prefilter mean", perf.Stages["prefilter"].MeanMs, 50.5, 1e-9)
	approx(t, "guards p99", perf.Stages["guards"].P99Ms, 1, 1e-9)
	if perf.ThroughputRPS != 0 {
		t.Fatalf("zero wall must yield zero throughput, got %v", perf.ThroughputRPS)
	}

	rounds := []*RoundResult{
		{Seed: 1, Perf: perf},
		{Seed: 2, Perf: perf},
		{Seed: 3}, // quality round: must be skipped, not zero-averaged
	}
	agg := AggregatePerf(rounds, 100)
	if agg == nil {
		t.Fatalf("AggregatePerf returned nil")
	}
	approx(t, "agg prefilter p50", agg.Stages["prefilter"].P50Ms, 50, 1e-9)
	if agg.Stages["prefilter"].Count != 200 {
		t.Fatalf("agg count = %d, want 200", agg.Stages["prefilter"].Count)
	}

	if AggregatePerf([]*RoundResult{{Seed: 1}}, 10) != nil {
		t.Fatalf("AggregatePerf over perf-less rounds must be nil")
	}
}

// TestProcOverheadSample exercises the sampler itself: counters must be
// monotonic, the heap gauges always present, and on Linux the procfs-derived
// values must be populated and non-negative. No timing-sensitive assertions.
func TestProcOverheadSample(t *testing.T) {
	begin := sampleProcUsage()
	// Burn a little CPU so the delta is measurable but do not assert on it —
	// a coarse CPU tick granularity may legitimately yield a zero delta.
	var x uint64
	for i := 0; i < 1000000; i++ {
		x += uint64(i) * 2654435761
	}
	runtime.KeepAlive(x)
	end := sampleProcUsage()

	if runtime.GOOS == "linux" {
		if !begin.cpuOK || !end.cpuOK {
			t.Fatalf("procfs cpu sample failed on linux: %+v -> %+v", begin, end)
		}
		if !end.rssOK {
			t.Fatalf("procfs peak-rss sample failed on linux: %+v", end)
		}
	}
	if end.cpuOK && begin.cpuOK && end.cpuSeconds < begin.cpuSeconds {
		t.Fatalf("cpu counter went backwards: %v -> %v", begin.cpuSeconds, end.cpuSeconds)
	}

	oh := overheadBetween(begin, end, 50*time.Millisecond)
	if oh == nil {
		t.Fatalf("overheadBetween returned nil")
	}
	if oh.HeapAllocMiB <= 0 || oh.HeapSysMiB < oh.HeapAllocMiB {
		t.Fatalf("implausible heap gauges: %+v", oh)
	}
	if oh.CPUSeconds < 0 {
		t.Fatalf("negative cpu seconds: %+v", oh)
	}
	if runtime.GOOS == "linux" && oh.PeakRSSMiB <= 0 {
		t.Fatalf("linux run must report a positive peak RSS: %+v", oh)
	}

	// Zero wall must not divide-by-zero into Inf/NaN.
	zero := overheadBetween(begin, end, 0)
	if zero.AvgCPUCores != 0 {
		t.Fatalf("zero wall must yield zero avg cores, got %v", zero.AvgCPUCores)
	}
}

// TestAggregatePerfOverhead hand-checks the cross-round overhead pooling: CPU
// seconds sum, peak RSS takes the max, heap gauges average, avg cores recompute
// from the pooled totals.
func TestAggregatePerfOverhead(t *testing.T) {
	mk := func(wall, cpu, peak, heapAlloc, heapSys float64) *RoundResult {
		return &RoundResult{Seed: 1, Perf: &PerfSummary{
			Stages:      map[string]*StageStats{"total": {}},
			WallSeconds: wall,
			Overhead:    &ProcessOverhead{CPUSeconds: cpu, PeakRSSMiB: peak, HeapAllocMiB: heapAlloc, HeapSysMiB: heapSys},
		}}
	}
	rounds := []*RoundResult{
		mk(2, 1.0, 100, 40, 80),
		mk(2, 0.5, 140, 60, 90),
		{Seed: 3}, // quality round: skipped
	}
	agg := AggregatePerf(rounds, 10)
	if agg == nil || agg.Overhead == nil {
		t.Fatalf("aggregate overhead missing: %+v", agg)
	}
	oh := agg.Overhead
	approx(t, "agg cpu seconds", oh.CPUSeconds, 1.5, 1e-9)
	approx(t, "agg avg cores", oh.AvgCPUCores, 1.5/4, 1e-9)
	approx(t, "agg peak rss", oh.PeakRSSMiB, 140, 1e-9)
	approx(t, "agg heap alloc", oh.HeapAllocMiB, 50, 1e-9)
	approx(t, "agg heap sys", oh.HeapSysMiB, 85, 1e-9)
}

// TestPerfSummaryOverheadJSON pins the report-contract field names of the
// overhead block.
func TestPerfSummaryOverheadJSON(t *testing.T) {
	perf := &PerfSummary{
		Stages:      map[string]*StageStats{"total": {}},
		WallSeconds: 1,
		Overhead: &ProcessOverhead{
			CPUSeconds:   0.5,
			AvgCPUCores:  0.5,
			PeakRSSMiB:   128,
			HeapAllocMiB: 64,
			HeapSysMiB:   96,
		},
	}
	data, err := json.Marshal(perf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"overhead"`, `"cpu_seconds"`, `"avg_cpu_cores"`, `"peak_rss_mib"`, `"heap_alloc_mib"`, `"heap_sys_mib"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("marshaled PerfSummary missing %s: %s", key, data)
		}
	}
}

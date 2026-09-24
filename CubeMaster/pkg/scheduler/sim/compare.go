// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// compare.go renders an A/B(/C...) comparison report for several schedsim
// runs of the SAME trace under different scheduler configs (typically one
// legacy config plus one profile per candidate policy). The first variant is
// the baseline; every other variant is reported as deltas against it.
//
// The rendering is a pure function over per-variant Reports so it is unit
// testable; the subprocess orchestration that produces those reports lives in
// cmd/schedsim/main.go.

// VariantReport binds a display name to one completed schedsim report.
type VariantReport struct {
	Name       string
	ConfigPath string
	Report     *Report
}

// metricDirection tells whether a higher or lower value is the improvement
// for a summary key. dirNA marks metrics whose "better" direction depends on
// the policy goal (allocation rates, active/empty node counts): per the
// design doc, cluster-level utilization alone cannot prove a packing policy
// is better, so these are reported without a verdict.
type metricDirection int

const (
	dirNA metricDirection = iota
	dirLowerBetter
	dirHigherBetter
)

// summaryDirections is an exact table over SummaryKeys (a closed set), not a
// substring heuristic.
var summaryDirections = map[string]metricDirection{
	"success_rate":         dirHigherBetter,
	"template_hit_rate":    dirHigherBetter,
	"jain_cpu":             dirHigherBetter,
	"jain_mem":             dirHigherBetter,
	"sched_latency_p50_ms": dirLowerBetter,
	"sched_latency_p95_ms": dirLowerBetter,
	"sched_latency_p99_ms": dirLowerBetter,
	"load_cv_cpu":          dirLowerBetter,
	"load_cv_mem":          dirLowerBetter,
	"fragmentation_ratio":  dirLowerBetter,
	"herding_top1_share":   dirLowerBetter,
}

// compareVerdictThreshold mirrors cube-bench compare: only |Δ%| >= 5% counts
// as a conclusion-worthy change.
const compareVerdictThreshold = 5.0

// variantStats aggregates one metric across a variant's rounds.
type variantStats struct {
	n    int
	mean float64
	sd   float64 // sample standard deviation; 0 when n < 2
	// ci is the half-width of the 95% confidence interval of the mean
	// (1.96·σ/√n, same convention as cube-bench compare); valid only when
	// hasCI is true (n ≥ 2).
	ci    float64
	hasCI bool
}

// aggregateVariant computes per-metric stats across the variant's rounds.
// With no rounds it falls back to the report's top-level summary as a single
// sample.
func aggregateVariant(v *VariantReport) map[string]variantStats {
	samples := make([]map[string]float64, 0, len(v.Report.Rounds))
	for _, r := range v.Report.Rounds {
		if r != nil && r.Summary != nil {
			samples = append(samples, r.Summary)
		}
	}
	if len(samples) == 0 && v.Report.Summary != nil {
		samples = append(samples, v.Report.Summary)
	}
	out := make(map[string]variantStats, len(SummaryKeys))
	for _, key := range SummaryKeys {
		vals := make([]float64, 0, len(samples))
		for _, s := range samples {
			if val, ok := s[key]; ok {
				vals = append(vals, val)
			}
		}
		if len(vals) == 0 {
			continue
		}
		st := variantStats{n: len(vals), mean: sampleMean(vals)}
		if st.n >= 2 {
			st.sd = sampleStdDev(vals)
			st.ci = 1.96 * st.sd / math.Sqrt(float64(st.n))
			st.hasCI = true
		}
		out[key] = st
	}
	return out
}

func sampleMean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func sampleStdDev(vs []float64) float64 {
	if len(vs) < 2 {
		return 0
	}
	allEqual := true
	for _, v := range vs[1:] {
		if v != vs[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		return 0
	}
	m := sampleMean(vs)
	var sum float64
	for _, v := range vs {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(vs)-1))
}

// compareRow is one metric's row across all variants.
type compareRow struct {
	key     string
	stats   []variantStats // indexed like the variants slice
	present []bool
	// deltaPct[i] is variant i's mean delta vs the baseline mean, in percent;
	// valid only when deltaOK[i] is true (baseline present and non-zero).
	deltaPct []float64
	deltaOK  []bool
}

func buildCompareRows(variants []VariantReport) []compareRow {
	aggs := make([]map[string]variantStats, len(variants))
	for i := range variants {
		aggs[i] = aggregateVariant(&variants[i])
	}
	rows := make([]compareRow, 0, len(SummaryKeys))
	for _, key := range SummaryKeys {
		row := compareRow{
			key:      key,
			stats:    make([]variantStats, len(variants)),
			present:  make([]bool, len(variants)),
			deltaPct: make([]float64, len(variants)),
			deltaOK:  make([]bool, len(variants)),
		}
		for i := range variants {
			st, ok := aggs[i][key]
			if !ok {
				continue
			}
			row.stats[i], row.present[i] = st, true
		}
		if row.present[0] && row.stats[0].mean != 0 {
			for i := 1; i < len(variants); i++ {
				if row.present[i] {
					row.deltaPct[i] = (row.stats[i].mean - row.stats[0].mean) / row.stats[0].mean * 100
					row.deltaOK[i] = true
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// RenderCompare renders the Markdown comparison report. variants must hold at
// least two entries; variants[0] is the baseline.
func RenderCompare(variants []VariantReport, generated time.Time) (string, error) {
	if len(variants) < 2 {
		return "", fmt.Errorf("sim: compare needs at least 2 variants, got %d", len(variants))
	}
	for i := range variants {
		if variants[i].Report == nil {
			return "", fmt.Errorf("sim: variant %q has no report", variants[i].Name)
		}
	}
	rows := buildCompareRows(variants)

	var b strings.Builder
	b.WriteString("# schedsim A/B Comparison Report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", generated.Format("2006-01-02 15:04:05 UTC"))

	base := variants[0].Report.Config
	b.WriteString("## Experiment Setup\n\n")
	b.WriteString("| field | value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| trace | `%s` |\n", base.Trace)
	fmt.Fprintf(&b, "| workload | %s |\n", base.Workload)
	fmt.Fprintf(&b, "| requests | %d |\n", base.Requests)
	fmt.Fprintf(&b, "| nodes | %d × %d millicores / %d MiB |\n", base.Nodes, base.NodeCPUMillis, base.NodeMemMiB)
	fmt.Fprintf(&b, "| instance_type | %s |\n", base.InstanceType)
	fmt.Fprintf(&b, "| template_preload | %s |\n", formatCompareNum(base.TemplatePreload))
	fmt.Fprintf(&b, "| allow_non_local_template | %t |\n", base.AllowNonLocalTemplate)
	fmt.Fprintf(&b, "| template_size_bytes | %d |\n", base.TemplateSizeBytes)
	fmt.Fprintf(&b, "| seed / rounds | %d / %d |\n", base.Seed, base.Rounds)
	mode := base.Mode
	if mode == "" {
		mode = "quality"
	}
	fmt.Fprintf(&b, "| mode | %s |\n", mode)
	// All variants are run by the same schedsim binary, so the code version is
	// a shared run property; empty for binaries built without VCS info.
	if base.Version != "" {
		fmt.Fprintf(&b, "| version | `%s` |\n", base.Version)
	}
	b.WriteString("\nVariants share the same trace, node fleet, preload draw seeds and round count; only the scheduler config differs.\n\n")
	b.WriteString("| role | name | config |\n| --- | --- | --- |\n")
	for i, v := range variants {
		role := "candidate"
		if i == 0 {
			role = "baseline"
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` |\n", role, v.Name, v.ConfigPath)
	}

	b.WriteString("\n## Metrics (mean across rounds; ± is the 95% CI half-width 1.96·σ/√n, shown when n = rounds ≥ 2)\n\n")
	var hdr strings.Builder
	hdr.WriteString("| metric")
	for _, v := range variants {
		fmt.Fprintf(&hdr, " | %s", v.Name)
	}
	hdr.WriteString(" |\n| ---")
	for range variants {
		hdr.WriteString(" | ---")
	}
	hdr.WriteString(" |\n")
	b.WriteString(hdr.String())
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s", row.key)
		for i := range variants {
			b.WriteString(" | ")
			if !row.present[i] {
				b.WriteString("—")
			} else if row.stats[i].hasCI {
				fmt.Fprintf(&b, "%s ± %s", formatCompareNum(row.stats[i].mean), formatCompareNum(row.stats[i].ci))
			} else {
				b.WriteString(formatCompareNum(row.stats[i].mean))
			}
		}
		b.WriteString(" |\n")
	}

	fmt.Fprintf(&b, "\n## Δ%% vs baseline (%s)\n\n", variants[0].Name)
	fmt.Fprintf(&b, "| metric")
	for _, v := range variants[1:] {
		fmt.Fprintf(&b, " | %s", v.Name)
	}
	b.WriteString(" |\n| ---")
	for range variants[1:] {
		b.WriteString(" | ---")
	}
	b.WriteString(" |\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s", row.key)
		for i := 1; i < len(variants); i++ {
			b.WriteString(" | ")
			if row.deltaOK[i] {
				fmt.Fprintf(&b, "%+.1f%%", row.deltaPct[i])
			} else {
				b.WriteString("—")
			}
		}
		b.WriteString(" |\n")
	}

	renderPerfSection(&b, variants)

	b.WriteString("\n## Conclusions\n\n")
	fmt.Fprintf(&b, "Only |Δ%%| ≥ %s with a known improvement direction is listed; metrics whose direction depends on the policy goal (allocation rates, node counts) never produce a verdict.\n",
		formatCompareNum(compareVerdictThreshold))
	for i := 1; i < len(variants); i++ {
		fmt.Fprintf(&b, "\n### %s vs %s\n\n", variants[i].Name, variants[0].Name)
		var improved, regressed []string
		for _, row := range rows {
			if !row.deltaOK[i] {
				continue
			}
			dir := summaryDirections[row.key]
			if dir == dirNA || math.Abs(row.deltaPct[i]) < compareVerdictThreshold {
				continue
			}
			better := (dir == dirHigherBetter) == (row.deltaPct[i] > 0)
			line := fmt.Sprintf("- **%s**: %+.1f%% (%s → %s)", row.key, row.deltaPct[i],
				formatCompareNum(row.stats[0].mean), formatCompareNum(row.stats[i].mean))
			if better {
				improved = append(improved, line)
			} else {
				regressed = append(regressed, line)
			}
		}
		b.WriteString("Improved:\n")
		writeCompareList(&b, improved)
		b.WriteString("Regressed:\n")
		writeCompareList(&b, regressed)
	}
	return b.String(), nil
}

func writeCompareList(b *strings.Builder, lines []string) {
	if len(lines) == 0 {
		b.WriteString("- (none)\n")
		return
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
}

// renderPerfSection appends the performance-mode stage table when the
// variants were run with --mode=performance. Values are the cross-round
// aggregates from each variant's report (means of per-round percentiles).
// Quality-mode reports carry no Perf block and the section is omitted.
func renderPerfSection(b *strings.Builder, variants []VariantReport) {
	anyPerf := false
	for i := range variants {
		if variants[i].Report.Perf != nil {
			anyPerf = true
		}
	}
	if !anyPerf {
		return
	}

	b.WriteString("\n## Scheduling overhead (performance mode; cross-round means)\n\n")
	fmt.Fprintf(b, "| stage metric")
	for _, v := range variants {
		fmt.Fprintf(b, " | %s", v.Name)
	}
	b.WriteString(" |\n| ---")
	for range variants {
		b.WriteString(" | ---")
	}
	b.WriteString(" |\n")
	perfRow := func(label string, get func(*PerfSummary) float64) {
		fmt.Fprintf(b, "| %s", label)
		for i := range variants {
			b.WriteString(" | ")
			perf := variants[i].Report.Perf
			if perf == nil {
				b.WriteString("—")
			} else {
				b.WriteString(formatCompareNum(get(perf)))
			}
		}
		b.WriteString(" |\n")
	}
	for _, stage := range PerfStageKeys {
		stage := stage
		perfRow(stage+"_p50_ms", func(p *PerfSummary) float64 { return p.Stages[stage].P50Ms })
		perfRow(stage+"_p95_ms", func(p *PerfSummary) float64 { return p.Stages[stage].P95Ms })
		perfRow(stage+"_p99_ms", func(p *PerfSummary) float64 { return p.Stages[stage].P99Ms })
	}
	perfRow("throughput_rps", func(p *PerfSummary) float64 { return p.ThroughputRPS })
	perfRow("wall_seconds", func(p *PerfSummary) float64 { return p.WallSeconds })

	// Process-level overhead (procfs CPU/RSS + Go heap); absent from reports
	// written before overhead sampling existed, and zero/omitted off Linux.
	overheadRow := func(label string, get func(*ProcessOverhead) float64) {
		fmt.Fprintf(b, "| %s", label)
		for i := range variants {
			b.WriteString(" | ")
			perf := variants[i].Report.Perf
			if perf == nil || perf.Overhead == nil {
				b.WriteString("—")
			} else {
				b.WriteString(formatCompareNum(get(perf.Overhead)))
			}
		}
		b.WriteString(" |\n")
	}
	overheadRow("proc_cpu_seconds", func(o *ProcessOverhead) float64 { return o.CPUSeconds })
	overheadRow("proc_avg_cpu_cores", func(o *ProcessOverhead) float64 { return o.AvgCPUCores })
	overheadRow("proc_peak_rss_mib", func(o *ProcessOverhead) float64 { return o.PeakRSSMiB })
	overheadRow("go_heap_alloc_mib", func(o *ProcessOverhead) float64 { return o.HeapAllocMiB })
	overheadRow("go_heap_sys_mib", func(o *ProcessOverhead) float64 { return o.HeapSysMiB })

	// Δ% vs baseline for the two headline overhead numbers.
	if variants[0].Report.Perf != nil {
		base := variants[0].Report.Perf
		fmt.Fprintf(b, "\nOverhead vs baseline (%s):", variants[0].Name)
		for i := 1; i < len(variants); i++ {
			perf := variants[i].Report.Perf
			if perf == nil {
				continue
			}
			p99Delta := "—"
			if base.Stages["total"] != nil && base.Stages["total"].P99Ms != 0 && perf.Stages["total"] != nil {
				p99Delta = fmt.Sprintf("%+.1f%%", (perf.Stages["total"].P99Ms-base.Stages["total"].P99Ms)/base.Stages["total"].P99Ms*100)
			}
			rpsDelta := "—"
			if base.ThroughputRPS != 0 {
				rpsDelta = fmt.Sprintf("%+.1f%%", (perf.ThroughputRPS-base.ThroughputRPS)/base.ThroughputRPS*100)
			}
			fmt.Fprintf(b, "\n- **%s**: total_p99 %s, throughput %s", variants[i].Name, p99Delta, rpsDelta)
		}
		b.WriteString("\n")
	}
}

// formatCompareNum renders integral values without decimals and everything
// else with up to 5 significant digits (same convention as cube-bench).
func formatCompareNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', 5, 64)
}

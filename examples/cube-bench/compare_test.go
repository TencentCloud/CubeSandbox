package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Flattening / sample extraction
// ---------------------------------------------------------------------------

func TestFlattenSummaryNested(t *testing.T) {
	obj := map[string]any{
		"total_time_s": 12.5,
		"success_rate": 0.9,
		"create": map[string]any{
			"p95_ms": 42.0,
			"stats":  map[string]any{"count": 3.0},
		},
		// non-numeric leaves are skipped
		"note":    "ignored",
		"ok":      true,
		"tags":    []any{1.0, 2.0},
		"nothing": nil,
	}
	out := map[string]float64{}
	flattenMetrics(obj, "", out)

	want := map[string]float64{
		"total_time_s":       12.5,
		"success_rate":       0.9,
		"create.p95_ms":      42,
		"create.stats.count": 3,
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("flattened metrics mismatch\n got: %v\nwant: %v", out, want)
	}
}

func TestFlattenRoundsAndSingleSample(t *testing.T) {
	dir := t.TempDir()

	// Simulator-style export: non-empty rounds array -> one sample per round.
	sim := writeTempFile(t, dir, "sim.json", `{
		"config": {"seed": 7, "workload": "mixed"},
		"summary": {"ignored_when_rounds_present": 1},
		"rounds": [
			{"seed": 1, "summary": {"a": 1, "b": {"c": 2}}},
			{"seed": 2, "summary": {"a": 3, "b": {"c": 4}}}
		]
	}`)
	f, err := loadSampleFile(sim)
	if err != nil {
		t.Fatalf("loadSampleFile(sim): %v", err)
	}
	if !f.viaRounds {
		t.Errorf("sim file should be marked viaRounds")
	}
	if len(f.samples) != 2 {
		t.Fatalf("sim file contributed %d samples, want 2", len(f.samples))
	}
	if want := map[string]float64{"a": 1, "b.c": 2}; !reflect.DeepEqual(f.samples[0], want) {
		t.Errorf("round 0 sample = %v, want %v", f.samples[0], want)
	}
	if want := map[string]float64{"a": 3, "b.c": 4}; !reflect.DeepEqual(f.samples[1], want) {
		t.Errorf("round 1 sample = %v, want %v", f.samples[1], want)
	}
	if f.config["seed"] != 7.0 {
		t.Errorf("config seed = %v, want 7", f.config["seed"])
	}

	// cube-bench-style export: no rounds -> the whole summary is one sample.
	plain := writeTempFile(t, dir, "plain.json", `{
		"config": {"template": "web"},
		"summary": {"x": 1}
	}`)
	f, err = loadSampleFile(plain)
	if err != nil {
		t.Fatalf("loadSampleFile(plain): %v", err)
	}
	if f.viaRounds {
		t.Errorf("plain file should not be marked viaRounds")
	}
	if len(f.samples) != 1 || !reflect.DeepEqual(f.samples[0], map[string]float64{"x": 1}) {
		t.Errorf("plain file samples = %v, want single {x:1}", f.samples)
	}

	// Real cube-bench exports keep the create/delete stat blocks at the top
	// level, outside "summary"; they must fold into the sample so latency
	// percentiles stay comparable.
	withLat := writeTempFile(t, dir, "with_latency.json", `{
		"summary": {"x": 1},
		"create": {"count": 2, "p95": 120.5},
		"delete": {"count": 2, "p95": 30}
	}`)
	f, err = loadSampleFile(withLat)
	if err != nil {
		t.Fatalf("loadSampleFile(withLat): %v", err)
	}
	wantLat := map[string]float64{"x": 1, "create.count": 2, "create.p95": 120.5, "delete.count": 2, "delete.p95": 30}
	if len(f.samples) != 1 || !reflect.DeepEqual(f.samples[0], wantLat) {
		t.Errorf("withLat samples = %v, want single %v", f.samples, wantLat)
	}

	// An empty rounds array falls back to the single-sample path.
	emptyRounds := writeTempFile(t, dir, "empty_rounds.json", `{"summary": {"x": 2}, "rounds": []}`)
	f, err = loadSampleFile(emptyRounds)
	if err != nil {
		t.Fatalf("loadSampleFile(emptyRounds): %v", err)
	}
	if f.viaRounds || len(f.samples) != 1 {
		t.Errorf("empty rounds should yield one sample, got viaRounds=%v n=%d", f.viaRounds, len(f.samples))
	}

	// Error paths.
	badJSON := writeTempFile(t, dir, "bad.json", `{not json`)
	if _, err := loadSampleFile(badJSON); err == nil {
		t.Errorf("expected parse error for bad.json")
	}
	noSummary := writeTempFile(t, dir, "no_summary.json", `{"config": {}}`)
	if _, err := loadSampleFile(noSummary); err == nil {
		t.Errorf("expected error for missing summary")
	}
	badRound := writeTempFile(t, dir, "bad_round.json", `{"rounds": [{"seed": 1}]}`)
	if _, err := loadSampleFile(badRound); err == nil {
		t.Errorf("expected error for round without summary")
	}
	if _, err := loadSampleFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

func TestCompareStatsAggregate(t *testing.T) {
	agg := aggregateSamples([]map[string]float64{
		{"a": 10, "b": 1},
		{"a": 12},
		{"a": 14, "b": 3},
	})

	a, ok := agg["a"]
	if !ok {
		t.Fatalf("metric a missing")
	}
	if a.n != 3 {
		t.Errorf("a.n = %d, want 3", a.n)
	}
	if a.mean != 12 {
		t.Errorf("a.mean = %v, want 12", a.mean)
	}
	// sample stddev of {10,12,14}: sqrt((4+0+4)/2) = 2
	if math.Abs(a.stdDev-2) > 1e-12 {
		t.Errorf("a.stdDev = %v, want 2", a.stdDev)
	}
	if !a.hasCI {
		t.Errorf("a.should have CI (n=3)")
	}
	if want := 1.96 * 2 / math.Sqrt(3); math.Abs(a.ci-want) > 1e-12 {
		t.Errorf("a.ci = %v, want %v", a.ci, want)
	}

	b, ok := agg["b"]
	if !ok {
		t.Fatalf("metric b missing")
	}
	if b.n != 2 || b.mean != 2 {
		t.Errorf("b = n:%d mean:%v, want n:2 mean:2 (samples without the key do not count)", b.n, b.mean)
	}
	if math.Abs(b.stdDev-math.Sqrt(2)) > 1e-12 {
		t.Errorf("b.stdDev = %v, want sqrt(2)", b.stdDev)
	}
	// 1.96 * sqrt(2) / sqrt(2) = 1.96 exactly
	if math.Abs(b.ci-1.96) > 1e-12 {
		t.Errorf("b.ci = %v, want 1.96", b.ci)
	}

	single := aggregateSamples([]map[string]float64{{"x": 5}})["x"]
	if single.n != 1 || single.mean != 5 {
		t.Errorf("single = n:%d mean:%v, want n:1 mean:5", single.n, single.mean)
	}
	if single.hasCI {
		t.Errorf("n=1 must not produce a CI")
	}
}

func TestCompareZeroBaselinePct(t *testing.T) {
	baseline := &sampleGroup{name: "b", samples: []map[string]float64{
		{"restart_rate": 0, "sched_cv": 1, "only_base": 3},
		{"restart_rate": 0, "sched_cv": 1, "only_base": 5},
	}}
	candidate := &sampleGroup{name: "c", samples: []map[string]float64{
		{"restart_rate": 0.5, "sched_cv": 1},
		{"restart_rate": 0.5, "sched_cv": 1},
	}}
	cmp := buildComparison(baseline, candidate, time.Now().UTC())

	rows := map[string]*compareRow{}
	for _, r := range cmp.rows {
		rows[r.key] = r
	}

	rr := rows["restart_rate"]
	if rr == nil {
		t.Fatalf("restart_rate row missing")
	}
	if rr.hasPct {
		t.Errorf("baseline mean is 0: Δ%% must be suppressed, got %v", rr.pct)
	}
	if got := pctCell(rr); got != "—" {
		t.Errorf("pctCell = %q, want em dash", got)
	}
	if rr.verdict != verdictImproved {
		t.Errorf("restart_rate verdict = %q, want improved (higher-better, delta>0)", rr.verdict)
	}

	cv := rows["sched_cv"]
	if cv == nil || cv.verdict != verdictSame {
		t.Errorf("sched_cv verdict = %v, want same (identical means)", cv)
	}

	ob := rows["only_base"]
	if ob == nil || ob.verdict != verdictMissing {
		t.Errorf("only_base verdict = %v, want missing (absent on candidate side)", ob)
	}

	// Rows without Δ% never enter the conclusions lists.
	improved, _, _ := cmp.conclusions()
	if len(improved) != 0 {
		t.Errorf("conclusions improved = %v, want empty (restart_rate has no Δ%%)", improved)
	}
}

// ---------------------------------------------------------------------------
// Direction table
// ---------------------------------------------------------------------------

func TestMetricDirection(t *testing.T) {
	cases := []struct {
		key  string
		want direction
	}{
		// higher-is-better
		{"success_rate", dirHigherBetter},
		{"hit_rate", dirHigherBetter},
		{"alloc_rate", dirHigherBetter},
		{"jain_fairness", dirHigherBetter},
		{"jain", dirHigherBetter},
		{"throughput_qps", dirHigherBetter},
		{"qps", dirHigherBetter},
		// lower-is-better
		{"latency_p99", dirLowerBetter},
		{"create.latency_p95", dirLowerBetter},
		{"api_delay", dirLowerBetter},
		{"job_duration", dirLowerBetter},
		{"error_rate", dirLowerBetter}, // lower rules beat "rate"
		{"failure_count", dirLowerBetter},
		{"sched_cv", dirLowerBetter},
		{"queue_cv_ratio", dirLowerBetter},
		{"fragmentation", dirLowerBetter},
		{"fragment_ratio", dirLowerBetter},
		{"herd_score", dirLowerBetter},
		{"herding_index", dirLowerBetter},
		// no direction: unknown keys, and false-positive guards
		{"strategy", dirNA},  // must not substring-match "rate"
		{"discovery", dirNA}, // must not substring-match "cv"
		{"total_time_s", dirNA},
		{"queue_depth", dirNA},
		{"create.p95_ms", dirNA},
	}
	for _, tc := range cases {
		if got := metricDirection(tc.key); got != tc.want {
			t.Errorf("metricDirection(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}
}

func TestCompareGroupPrefix(t *testing.T) {
	cases := map[string]string{
		"create.p95_ms":     "create",
		"delete.p99_ms":     "delete",
		"queue_depth":       "queue",
		"sched_latency_p99": "sched",
		"load_balance_cv":   "load",
		"jain":              "jain",
	}
	for key, want := range cases {
		if got := groupPrefix(key); got != want {
			t.Errorf("groupPrefix(%q) = %q, want %q", key, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end
// ---------------------------------------------------------------------------

func TestCompareEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// Baseline: one cube-bench-style export (single sample) + one
	// simulator-style export (two samples via rounds).
	b1 := writeTempFile(t, dir, "old1.json", `{
		"config": {"seed": 1, "rounds": 3, "workload": "mixed", "version": "v1"},
		"summary": {
			"success_rate": 0.9, "throughput_qps": 100, "error_rate": 0.1,
			"queue_depth": 4, "sched_latency_p95": 120, "create": {"p95_ms": 50}
		}
	}`)
	b2 := writeTempFile(t, dir, "old2.json", `{
		"config": {"workload": "mixed", "version": "v1"},
		"rounds": [
			{"seed": 11, "summary": {"success_rate": 0.92, "throughput_qps": 110, "error_rate": 0.08, "queue_depth": 5, "sched_latency_p95": 130, "create": {"p95_ms": 60}}},
			{"seed": 12, "summary": {"success_rate": 0.94, "throughput_qps": 90, "error_rate": 0.12, "queue_depth": 6, "sched_latency_p95": 110, "create": {"p95_ms": 70}}}
		]
	}`)
	c1 := writeTempFile(t, dir, "new1.json", `{
		"config": {"workload": "mixed", "version": "v2"},
		"summary": {
			"success_rate": 0.99, "throughput_qps": 90, "error_rate": 0.05,
			"queue_depth": 7, "sched_latency_p95": 100, "create": {"p95_ms": 45}
		}
	}`)
	c2 := writeTempFile(t, dir, "new2.json", `{
		"config": {"workload": "mixed", "version": "v2"},
		"rounds": [
			{"seed": 21, "summary": {"success_rate": 0.97, "throughput_qps": 95, "error_rate": 0.05, "queue_depth": 7, "sched_latency_p95": 90, "create": {"p95_ms": 40}}},
			{"seed": 22, "summary": {"success_rate": 0.98, "throughput_qps": 85, "error_rate": 0.05, "queue_depth": 7, "sched_latency_p95": 95, "create": {"p95_ms": 50}}}
		]
	}`)

	out := filepath.Join(dir, "report.md")
	var buf bytes.Buffer
	err := runCompare([]string{
		"--baseline", b1 + "," + b2,
		"--candidate", c1 + "," + c2,
		"--baseline-name", "default",
		"--candidate-name", "new-policy",
		"-o", out,
	}, &buf)
	if err != nil {
		t.Fatalf("runCompare: %v", err)
	}

	printed := buf.String()
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(printed, string(saved)) {
		t.Errorf("printed output does not contain the saved report")
	}
	t.Logf("generated report:\n%s", saved)

	report := string(saved)

	// Metadata.
	for _, want := range []string{
		"# A/B Comparison Report",
		"Generated:",
		"| baseline | default | 2 | 3 |",
		"| candidate | new-policy | 2 | 3 |",
		"(n=1)",
		"(n=2 via rounds)",
		"seed=1",
		"workload=mixed",
		"version=v2",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing metadata %q", want)
		}
	}

	// Comparison table: metric rows, deltas, verdicts.
	for _, want := range []string{
		"## Metric Comparison",
		"### create",
		"### sched",
		"| success_rate | 0.92 ± 0.022632 | 0.98 ± 0.011316 | +0.06 | +6.5% | improved |",
		"| error_rate | 0.1 ± 0.022632 | 0.05 ± 0 | -0.05 | -50.0% | improved |",
		"| sched_latency_p95 | 120 ± 11.316 | 95 ± 5.658 | -25 | -20.8% | improved |",
		"| throughput_qps | 100 ± 11.316 | 90 ± 5.658 | -10 | -10.0% | regressed |",
		"| queue_depth | 5 ± 1.1316 | 7 ± 0 | +2 | +40.0% | n/a |",
		// create.p95_ms carries no direction keyword -> n/a per the direction table
		"| create.p95_ms | 60 ± 11.316 | 45 ± 5.658 | -15 | -25.0% | n/a |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing table content %q", want)
		}
	}

	// Conclusions.
	for _, want := range []string{
		"## Conclusions",
		"### Improved (|Δ%| ≥ 5%)",
		"- **error_rate**: -50.0% (0.1 → 0.05)",
		"- **sched_latency_p95**: -20.8% (120 → 95)",
		"- **success_rate**: +6.5% (0.92 → 0.98)",
		"### Regressed (|Δ%| ≥ 5%)",
		"- **throughput_qps**: -10.0% (100 → 90)",
		"### No direction (n/a)",
		"- **create.p95_ms**: -25.0% (60 → 45)",
		"- **queue_depth**: +40.0% (5 → 7)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing conclusion %q", want)
		}
	}

	// Improved entries are ordered by |Δ%| descending.
	iErr := strings.Index(report, "**error_rate**")
	iSched := strings.Index(report, "**sched_latency_p95**")
	iSuccess := strings.Index(report, "**success_rate**")
	if !(iErr >= 0 && iErr < iSched && iSched < iSuccess) {
		t.Errorf("improved list not ordered by |Δ%%| desc: error_rate@%d sched@%d success@%d", iErr, iSched, iSuccess)
	}

	// regressed conclusion entry must appear after its heading, n/a entry after its own.
	if strings.Index(report, "**throughput_qps**: -10.0%") < strings.Index(report, "### Regressed") {
		t.Errorf("throughput_qps conclusion not under the Regressed heading")
	}
	if strings.Index(report, "**queue_depth**") < strings.Index(report, "### No direction") {
		t.Errorf("queue_depth conclusion not under the No direction heading")
	}
}

func TestCompareFlagErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := runCompare(nil, &buf); err == nil {
		t.Errorf("expected error when --baseline/--candidate are missing")
	}
	if err := runCompare([]string{"--bogus"}, &buf); err == nil {
		t.Errorf("expected error for unknown flag")
	}
	buf.Reset()
	if err := runCompare([]string{"--help"}, &buf); err != nil {
		t.Errorf("--help should return nil, got %v", err)
	}
	if !strings.Contains(buf.String(), "--baseline") {
		t.Errorf("usage text should mention --baseline")
	}
}

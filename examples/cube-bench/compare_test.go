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
	// n=3 -> df=2 -> t95 = 4.303; the small-sample interval is much wider
	// than the normal 1.96·σ/√n.
	if want := 4.303 * 2 / math.Sqrt(3); math.Abs(a.ci-want) > 1e-12 {
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
	// n=2 -> df=1 -> t95 = 12.706; 12.706 * sqrt(2) / sqrt(2) = 12.706 exactly
	if math.Abs(b.ci-12.706) > 1e-12 {
		t.Errorf("b.ci = %v, want 12.706", b.ci)
	}

	single := aggregateSamples([]map[string]float64{{"x": 5}})["x"]
	if single.n != 1 || single.mean != 5 {
		t.Errorf("single = n:%d mean:%v, want n:1 mean:5", single.n, single.mean)
	}
	if single.hasCI {
		t.Errorf("n=1 must not produce a CI")
	}
}

func TestT95(t *testing.T) {
	// Table values (df <= 30) are exact; beyond the table the Cornish–Fisher
	// expansion must track the true quantile instead of jumping to 1.96.
	cases := []struct {
		n    int
		want float64
		tol  float64
	}{
		{2, 12.706, 1e-12},
		{31, 2.042, 1e-12},  // df=30, last table entry
		{32, 2.0395, 1e-3},  // df=31: true 2.03951
		{61, 2.0003, 1e-3},  // df=60: true 2.00030
		{121, 1.9799, 1e-3}, // df=120: true 1.97993
		{10000, 1.9602, 1e-3},
	}
	for _, tc := range cases {
		if got := t95(tc.n); math.Abs(got-tc.want) > tc.tol {
			t.Errorf("t95(%d) = %v, want ~%v", tc.n, got, tc.want)
		}
	}
	// The quantile decreases monotonically in df.
	for n := 2; n < 500; n++ {
		if t95(n) < t95(n+1) {
			t.Fatalf("t95 not monotonic: t95(%d)=%v < t95(%d)=%v", n, t95(n), n+1, t95(n+1))
		}
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
	if rr.verdict != verdictRegressed {
		t.Errorf("restart_rate verdict = %q, want regressed (restart_* rates are lower-better, delta>0)", rr.verdict)
	}

	cv := rows["sched_cv"]
	if cv == nil || cv.verdict != verdictSame {
		t.Errorf("sched_cv verdict = %v, want same (identical means)", cv)
	}

	ob := rows["only_base"]
	if ob == nil || ob.verdict != verdictMissing {
		t.Errorf("only_base verdict = %v, want missing (absent on candidate side)", ob)
	}

	// Zero-baseline rows fall back to the absolute delta: 0 → 0.5 with tight
	// CIs (both sides identical values ⇒ CI 0) is a flagged regression even
	// though Δ% is suppressed in the table.
	improved, regressed, noDir := cmp.conclusions()
	if len(improved) != 0 {
		t.Errorf("conclusions improved = %v, want empty", improved)
	}
	if len(regressed) != 1 || regressed[0].key != "restart_rate" {
		t.Errorf("conclusions regressed = %v, want [restart_rate] (zero-baseline absolute-delta fallback)", regressed)
	}
	if len(noDir) != 0 {
		t.Errorf("conclusions noDir = %v, want empty (only_base misses the candidate side; sched_cv is unchanged)", noDir)
	}

	// The flagged zero-baseline row must carry the caveat marker in the
	// rendered report so the report stands alone without the README.
	if report := renderComparison(cmp); !strings.Contains(report, "zero baseline") {
		t.Errorf("rendered report missing the zero-baseline marker:\n%s", report)
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
		{"restart_rate", dirLowerBetter},    // rising bad rates are regressions
		{"preempt_rate", dirLowerBetter},    // even though "rate" is a
		{"preemption_rate", dirLowerBetter}, // higher-better token
		{"eviction_rate", dirLowerBetter},
		{"retry_rate", dirLowerBetter},
		{"sched_cv", dirLowerBetter},
		{"queue_cv_ratio", dirLowerBetter},
		{"fragmentation", dirLowerBetter},
		{"fragment_ratio", dirLowerBetter},
		{"herd_score", dirLowerBetter},
		{"herding_index", dirLowerBetter},
		// cube-bench stat blocks: any stat suffix under create/delete is
		// lower-better, with or without a "_ms" suffix
		{"create.p95", dirLowerBetter},
		{"create.p95_ms", dirLowerBetter},
		{"delete.avg", dirLowerBetter},
		// no direction: unknown keys, and false-positive guards
		{"strategy", dirNA},  // must not substring-match "rate"
		{"discovery", dirNA}, // must not substring-match "cv"
		{"total_time_s", dirNA},
		{"queue_depth", dirNA},
		{"create.count", dirNA}, // counts are not a quality signal
		{"summary.errors", dirNA},
		{"per_template.tpl-a.attempts", dirNA},
		// Data-derived key segments must not flip classification: the
		// template id in per_template.<id>.<stat> is user data.
		{"per_template.sbx-errors.success_rate", dirHigherBetter},
		{"per_template.herd-pool.success_rate", dirHigherBetter},
		{"per_template.pool.with.dots.error_rate", dirLowerBetter},
		{"per_template.tpl-a.created", dirNA},
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
		// per_template keys group by the embedded template id
		"per_template.tpl-a.success_rate": "per_template.tpl-a",
		"per_template.tpl-b.success_rate": "per_template.tpl-b",
		"per_template.tpl-a":              "per_template.tpl-a",
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

// TestCompareMixedShapesEndToEnd: a create-only export mixed into a
// create-delete group must surface the per-metric sample-count note (and the
// conclusion annotation) in the rendered report.
func TestCompareMixedShapesEndToEnd(t *testing.T) {
	dir := t.TempDir()

	b1 := writeTempFile(t, dir, "base-full.json", `{
		"summary": {"success_rate": 0.9},
		"create": {"p95_ms": 50}, "delete": {"p95_ms": 20}
	}`)
	b2 := writeTempFile(t, dir, "base-create-only.json", `{
		"summary": {"success_rate": 0.92},
		"create": {"p95_ms": 55}
	}`)
	c1 := writeTempFile(t, dir, "cand-full.json", `{
		"summary": {"success_rate": 0.95},
		"create": {"p95_ms": 40}, "delete": {"p95_ms": 30}
	}`)

	out := filepath.Join(dir, "report.md")
	var buf bytes.Buffer
	err := runCompare([]string{"--baseline", b1 + "," + b2, "--candidate", c1, "-o", out}, &buf)
	if err != nil {
		t.Fatalf("runCompare: %v", err)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(saved)

	for _, want := range []string{
		"Some metrics appear in fewer samples than the group header count",
		"- `delete.p95_ms`: n=1 of 2 baseline samples",
		// delete.p95_ms went 20 → 30 (+50%): flagged regressed (lower-better),
		// annotated with the reduced baseline n.
		"- **delete.p95_ms**: +50.0% (20 → 30) (baseline n=1/2)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}
}

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
		"| success_rate | 0.92 ± 0.049687 | 0.98 ± 0.024843 | +0.06 | +6.5% | improved |",
		"| error_rate | 0.1 ± 0.049687 | 0.05 ± 0 | -0.05 | -50.0% | improved |",
		"| sched_latency_p95 | 120 ± 24.843 | 95 ± 12.422 | -25 | -20.8% | improved |",
		"| throughput_qps | 100 ± 24.843 | 90 ± 12.422 | -10 | -10.0% | regressed |",
		"| queue_depth | 5 ± 2.4843 | 7 ± 0 | +2 | +40.0% | n/a |",
		// cube-bench stat-block keys (create.p95_ms etc.) are lower-better
		// per the direction table
		"| create.p95_ms | 60 ± 24.843 | 45 ± 12.422 | -15 | -25.0% | improved |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing table content %q", want)
		}
	}

	// Conclusions: with n=3 per side the difference-CI gate keeps error_rate
	// (|Δ| = 0.05 > √(0.049687² + 0²)) and success_rate (0.06 >
	// √(0.049687² + 0.024843²) ≈ 0.0556); the other directional deltas are
	// within noise and stay out of the lists.
	for _, want := range []string{
		"## Conclusions",
		"### Improved (|Δ%| ≥ 5%, beyond 95% CI of the difference when n ≥ 2)",
		"- **error_rate**: -50.0% (0.1 → 0.05)",
		"- **success_rate**: +6.5% (0.92 → 0.98)",
		"### Regressed (|Δ%| ≥ 5%, beyond 95% CI of the difference when n ≥ 2)",
		"- (none)",
		"### No direction (n/a, only |Δ%| ≥ 5% or newly-nonzero shown)",
		"- **queue_depth**: +40.0% (5 → 7)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing conclusion %q", want)
		}
	}

	// CI-gated deltas must not appear as conclusions.
	for _, notWant := range []string{
		"- **sched_latency_p95**:",
		"- **throughput_qps**:",
		"- **create.p95_ms**:",
	} {
		if strings.Contains(report, notWant) {
			t.Errorf("CI-gated metric %q must not appear in conclusions", notWant)
		}
	}

	// The single surviving improved entry must appear under its own heading,
	// the n/a entry under its own.
	iErr := strings.Index(report, "**error_rate**")
	if iErr < strings.Index(report, "### Improved") {
		t.Errorf("error_rate conclusion not under the Improved heading")
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

func TestSampleCountNotes(t *testing.T) {
	// Heterogeneous exports: delete.p95_ms is absent from the second baseline
	// sample, queue_depth is absent from the whole candidate side, and
	// success_rate is present in every sample on both sides.
	baseline := &sampleGroup{
		name: "base",
		samples: []map[string]float64{
			{"success_rate": 0.9, "delete.p95_ms": 40, "queue_depth": 5},
			{"success_rate": 0.92, "queue_depth": 6},
		},
	}
	candidate := &sampleGroup{
		name: "cand",
		samples: []map[string]float64{
			{"success_rate": 0.95, "delete.p95_ms": 30},
			{"success_rate": 0.96, "delete.p95_ms": 28},
		},
	}
	cmp := buildComparison(baseline, candidate, time.Now())

	notes := cmp.sampleCountNotes()
	if len(notes) != 1 {
		t.Fatalf("sampleCountNotes len = %d, want 1: %v", len(notes), notes)
	}
	want := "- `delete.p95_ms`: n=1 of 2 baseline samples"
	if notes[0] != want {
		t.Errorf("note = %q, want %q", notes[0], want)
	}

	// Missing-on-one-side rows (verdictMissing) have no n on that side and
	// must not produce a note for that side.
	for _, note := range notes {
		if strings.Contains(note, "queue_depth") || strings.Contains(note, "success_rate") {
			t.Errorf("unexpected note %q", note)
		}
	}

	// Homogeneous samples produce no notes at all.
	same := &sampleGroup{
		name: "same",
		samples: []map[string]float64{
			{"success_rate": 0.9},
			{"success_rate": 0.91},
		},
	}
	if got := buildComparison(same, same, time.Now()).sampleCountNotes(); len(got) != 0 {
		t.Errorf("homogeneous comparison produced notes: %v", got)
	}
}

func TestCompareDispatchSaturatedWarning(t *testing.T) {
	dir := t.TempDir()

	b1 := writeTempFile(t, dir, "base-saturated.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p50_ms": 500, "dispatch_saturated": 1}
	}`)
	c1 := writeTempFile(t, dir, "cand-clean.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p50_ms": 3}
	}`)

	out := filepath.Join(dir, "report.md")
	var buf bytes.Buffer
	if err := runCompare([]string{"--baseline", b1, "--candidate", c1, "-o", out}, &buf); err != nil {
		t.Fatalf("runCompare: %v", err)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(saved)

	if !strings.Contains(report, "> **Warning:** baseline (") {
		t.Errorf("report missing the saturation warning naming the baseline group\n%s", report)
	}
	// The marker is a run-quality flag, not a metric: it must not survive
	// into the comparison table as a row.
	if strings.Contains(report, "| dispatch_saturated |") {
		t.Errorf("dispatch_saturated leaked into the metric table\n%s", report)
	}
}

func TestComparePartialWarning(t *testing.T) {
	dir := t.TempDir()

	b1 := writeTempFile(t, dir, "base-partial.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p50_ms": 500, "partial": 1}
	}`)
	c1 := writeTempFile(t, dir, "cand-clean.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p50_ms": 3}
	}`)

	out := filepath.Join(dir, "report.md")
	var buf bytes.Buffer
	if err := runCompare([]string{"--baseline", b1, "--candidate", c1, "-o", out}, &buf); err != nil {
		t.Fatalf("runCompare: %v", err)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(saved)

	if !strings.Contains(report, "> **Note:** baseline (") {
		t.Errorf("report missing the partial note naming the baseline group\n%s", report)
	}
	// Like dispatch_saturated, the marker is a run-quality flag and must not
	// appear as a comparison row.
	if strings.Contains(report, "| partial |") {
		t.Errorf("partial leaked into the metric table\n%s", report)
	}
}

func TestLoadSampleFileDropsDispatchKeysWhenFlagged(t *testing.T) {
	dir := t.TempDir()

	path := writeTempFile(t, dir, "flagged.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p50_ms": 500, "dispatch_qps": 12.5, "dispatch_saturated": 1, "per_template.tpl-a.created": 40}
	}`)
	f, err := loadSampleFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.saturated {
		t.Fatal("saturated flag not recorded")
	}
	// The flagged run's queue_delay_*/dispatch_* values describe the client
	// semaphore, not the offered schedule: they must be dropped before the
	// sample reaches the metric tables and verdict lists. per_template.*
	// blocks survive a saturated run — they reflect what actually ran.
	for _, k := range []string{"queue_delay_p50_ms", "dispatch_qps", "dispatch_saturated"} {
		if _, ok := f.samples[0][k]; ok {
			t.Errorf("key %q survived the flagged-sample drop", k)
		}
	}
	if got := f.samples[0]["success_rate"]; got != 0.9 {
		t.Errorf("success_rate = %v, want 0.9 (unrelated keys kept)", got)
	}
	if got := f.samples[0]["per_template.tpl-a.created"]; got != 40 {
		t.Errorf("per_template.tpl-a.created = %v, want 40 (kept for a saturated run)", got)
	}

	path = writeTempFile(t, dir, "partial.json", `{
		"summary": {"success_rate": 0.9, "queue_delay_p99_ms": 42, "partial": 1, "per_template.tpl-a.created": 7}
	}`)
	f, err = loadSampleFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.partial {
		t.Fatal("partial flag not recorded")
	}
	if _, ok := f.samples[0]["queue_delay_p99_ms"]; ok {
		t.Errorf("queue_delay_p99_ms survived the partial-sample drop")
	}
	// A partial run's per_template.* blocks aggregate only the results
	// consumed before the quit, so they must be dropped too.
	if _, ok := f.samples[0]["per_template.tpl-a.created"]; ok {
		t.Errorf("per_template.tpl-a.created survived the partial-sample drop")
	}
}

func TestCompareNoDirSortedByImpact(t *testing.T) {
	// Three directionless metrics: one small pct move, one large pct move, one
	// zero-baseline row (no Δ%). Expect large pct first, then small pct, then
	// the zero-baseline row ranked by absolute delta rather than buried.
	baseline := &sampleGroup{name: "b", samples: []map[string]float64{
		{"aaa_small": 100, "zzz_big": 100, "mmm_zero": 0},
	}}
	candidate := &sampleGroup{name: "c", samples: []map[string]float64{
		{"aaa_small": 106, "zzz_big": 160, "mmm_zero": 0.5},
	}}
	cmp := buildComparison(baseline, candidate, time.Now().UTC())

	_, _, noDir := cmp.conclusions()
	if len(noDir) != 3 {
		t.Fatalf("noDir = %v, want 3 rows", noDir)
	}
	if noDir[0].key != "zzz_big" || noDir[1].key != "aaa_small" || noDir[2].key != "mmm_zero" {
		t.Errorf("noDir order = [%s %s %s], want [zzz_big aaa_small mmm_zero]",
			noDir[0].key, noDir[1].key, noDir[2].key)
	}
}

func TestCompareScaleCountersNotConcluded(t *testing.T) {
	// Different workload sizes (-n) move create.count by -40% and
	// summary.errors likewise — pure scale noise that must not surface in any
	// conclusion list, even the no-direction one.
	baseline := &sampleGroup{name: "b", samples: []map[string]float64{
		{"create.count": 100, "summary.errors": 5, "failure_count": 10},
	}}
	candidate := &sampleGroup{name: "c", samples: []map[string]float64{
		{"create.count": 60, "summary.errors": 3, "failure_count": 20},
	}}
	cmp := buildComparison(baseline, candidate, time.Now().UTC())

	improved, regressed, noDir := cmp.conclusions()
	for _, list := range [][]*compareRow{improved, regressed, noDir} {
		for _, r := range list {
			if r.key == "create.count" || r.key == "summary.errors" {
				t.Errorf("scale counter %s leaked into conclusions", r.key)
			}
		}
	}
	// failure_count is NOT a scale counter: "count" outside a create/delete
	// stat block stays directional, so 10 -> 20 is a flagged regression.
	if len(regressed) != 1 || regressed[0].key != "failure_count" {
		t.Errorf("regressed = %v, want [failure_count]", regressed)
	}
}

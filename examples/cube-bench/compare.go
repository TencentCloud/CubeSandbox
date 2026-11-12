package main

// compare.go implements `cube-bench compare`, an A/B comparison report
// generator for scheduling-policy experiments. It reads two groups of
// cube-bench or simulator JSON exports (each file contributes one sample, or
// one sample per entry of a non-empty top-level "rounds" array), aggregates
// every numeric metric across samples (mean/stddev/95% CI), and renders a
// markdown report with per-metric deltas and improvement/regression verdicts.
//
// The entry point is runCompare; wiring it into the CLI is a one-liner in
// main.go:
//
//	if len(os.Args) > 1 && os.Args[1] == "compare" {
//		if err := runCompare(os.Args[2:], os.Stdout); err != nil {
//			fmt.Fprintln(os.Stderr, "ERROR:", err)
//			os.Exit(1)
//		}
//		return
//	}

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const compareUsage = `Usage: cube-bench compare --baseline f1.json,f2.json --candidate g1.json,g2.json [flags]

Generates an A/B comparison report from two groups of cube-bench or
simulator JSON exports. Each file contributes one sample, or one sample per
entry when it carries a non-empty top-level "rounds" array.

Flags:
  --baseline files        Comma-separated baseline result files (required)
  --candidate files       Comma-separated candidate result files (required)
  --baseline-name name    Label for the baseline group (default "baseline")
  --candidate-name name   Label for the candidate group (default "candidate")
  -o, --output file       Also write the markdown report to this file
`

// runCompare is the testable entry point of the compare subcommand: it parses
// args (without touching the global flag set), writes the markdown report to
// stdout, and optionally also to the file given via -o/--output.
func runCompare(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are returned, not printed

	var baselineList, candidateList, baselineName, candidateName, output string
	fs.StringVar(&baselineList, "baseline", "", "Comma-separated baseline result files")
	fs.StringVar(&candidateList, "candidate", "", "Comma-separated candidate result files")
	fs.StringVar(&baselineName, "baseline-name", "baseline", "Label for the baseline group")
	fs.StringVar(&candidateName, "candidate-name", "candidate", "Label for the candidate group")
	fs.StringVar(&output, "o", "", "Also write the markdown report to this file")
	fs.StringVar(&output, "output", "", "Also write the markdown report to this file")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, compareUsage)
			return nil
		}
		return fmt.Errorf("compare: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("compare: unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	baselinePaths := splitCommaList(baselineList)
	candidatePaths := splitCommaList(candidateList)
	if len(baselinePaths) == 0 || len(candidatePaths) == 0 {
		return errors.New("compare: both --baseline and --candidate file lists are required")
	}

	baseline, err := loadSampleGroup(baselineName, baselinePaths)
	if err != nil {
		return err
	}
	candidate, err := loadSampleGroup(candidateName, candidatePaths)
	if err != nil {
		return err
	}

	cmp := buildComparison(baseline, candidate, time.Now().UTC())
	report := renderComparison(cmp)

	if output != "" {
		if err := os.WriteFile(output, []byte(report), 0644); err != nil {
			return fmt.Errorf("compare: write report %s: %w", output, err)
		}
	}
	fmt.Fprint(stdout, report)
	if output != "" {
		fmt.Fprintf(stdout, "\nReport saved to %s\n", output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Input parsing: files -> samples
// ---------------------------------------------------------------------------

// sampleFile is one input JSON file: its config block plus the metric samples
// it contributes (one per round when a non-empty "rounds" array is present,
// otherwise a single sample built from the top-level "summary" merged with the
// top-level aggregate stat blocks such as "create"/"delete").
type sampleFile struct {
	path      string
	config    map[string]any
	samples   []map[string]float64
	viaRounds bool
}

// sampleGroup is one side of the comparison (baseline or candidate).
type sampleGroup struct {
	name    string
	files   []*sampleFile
	samples []map[string]float64
}

func splitCommaList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadSampleGroup(name string, paths []string) (*sampleGroup, error) {
	g := &sampleGroup{name: name}
	for _, p := range paths {
		f, err := loadSampleFile(p)
		if err != nil {
			return nil, err
		}
		g.files = append(g.files, f)
		g.samples = append(g.samples, f.samples...)
	}
	return g, nil
}

func loadSampleFile(path string) (*sampleFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compare: read %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("compare: parse %s: %w", path, err)
	}

	f := &sampleFile{path: path}
	if cfg, ok := root["config"].(map[string]any); ok {
		f.config = cfg
	}

	// A non-empty top-level "rounds" array turns the file into one sample
	// per round; otherwise the top-level "summary" is the single sample.
	if rounds, ok := root["rounds"].([]any); ok && len(rounds) > 0 {
		f.viaRounds = true
		for i, r := range rounds {
			rm, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("compare: %s: rounds[%d] is not an object", path, i)
			}
			sample, err := flattenSummaryValue(rm["summary"])
			if err != nil {
				return nil, fmt.Errorf("compare: %s: rounds[%d]: %w", path, i, err)
			}
			f.samples = append(f.samples, sample)
		}
		return f, nil
	}

	sample, err := flattenSummaryValue(root["summary"])
	if err != nil {
		return nil, fmt.Errorf("compare: %s: %w", path, err)
	}
	// cube-bench exports keep the run-wide create/delete latency percentile
	// blocks at the top level, outside "summary". Merge them into the sample
	// so A/B reports of real runs keep their primary latency metrics; keys
	// already present in "summary" keep precedence. Round-style (simulator)
	// files are untouched: their top level carries no per-run aggregates.
	mergeRootStatBlocks(root, sample)
	f.samples = []map[string]float64{sample}
	return f, nil
}

// rootStatBlocks are the aggregate blocks cube-bench writes at the top level
// of an export, outside "summary": "create" and "delete" hold the run-wide
// latency percentiles (count/min/max/avg/std/p50/p90/p95/p99). Merged under
// their dotted prefix they surface as e.g. "create.p95_ms".
var rootStatBlocks = []string{"create", "delete"}

// mergeRootStatBlocks flattens the top-level aggregate stat blocks of a
// cube-bench export into sample, filling only keys that are absent from it
// (the summary keeps precedence, so hand-written files stay stable).
func mergeRootStatBlocks(root map[string]any, sample map[string]float64) {
	for _, block := range rootStatBlocks {
		obj, ok := root[block].(map[string]any)
		if !ok {
			continue
		}
		blockVals := make(map[string]float64)
		flattenMetrics(obj, block, blockVals)
		for k, v := range blockVals {
			if _, exists := sample[k]; !exists {
				sample[k] = v
			}
		}
	}
}

func flattenSummaryValue(v any) (map[string]float64, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("missing or non-object \"summary\"")
	}
	out := make(map[string]float64)
	flattenMetrics(obj, "", out)
	return out, nil
}

// flattenMetrics walks obj recursively and records every numeric leaf under a
// dotted key (e.g. "create.p95_ms"). Non-numeric leaves (strings, booleans,
// arrays, nulls) are skipped.
func flattenMetrics(obj map[string]any, prefix string, out map[string]float64) {
	for k, v := range obj {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch t := v.(type) {
		case map[string]any:
			flattenMetrics(t, key, out)
		case float64:
			out[key] = t
		}
	}
}

// ---------------------------------------------------------------------------
// Aggregation across samples
// ---------------------------------------------------------------------------

type metricStats struct {
	n      int
	mean   float64
	stdDev float64 // sample standard deviation (n-1 denominator)
	ci     float64 // half-width of the 95% confidence interval
	hasCI  bool    // false when n < 2
}

func aggregateSamples(samples []map[string]float64) map[string]metricStats {
	values := make(map[string][]float64)
	for _, s := range samples {
		for k, v := range s {
			values[k] = append(values[k], v)
		}
	}
	out := make(map[string]metricStats, len(values))
	for k, vs := range values {
		st := metricStats{n: len(vs), mean: sampleMean(vs), stdDev: sampleStdDev(vs)}
		if st.n >= 2 {
			st.ci = tCrit95(st.n) * st.stdDev / math.Sqrt(float64(st.n))
			st.hasCI = true
		}
		out[k] = st
	}
	return out
}

// tCrit95Table holds the two-sided 95% critical values of Student's t,
// indexed by degrees of freedom (df = n-1).
var tCrit95Table = [...]float64{ // index = df-1
	12.706205, // df=1  (n=2)
	4.302653,  // df=2  (n=3)
	3.182446,  // df=3
	2.776445,  // df=4
	2.570582,  // df=5
	2.446912,  // df=6
	2.364624,  // df=7
	2.306004,  // df=8
	2.262157,  // df=9
	2.228139,  // df=10
	2.200985,  // df=11
	2.178813,  // df=12
	2.160369,  // df=13
	2.144787,  // df=14
	2.131450,  // df=15
	2.119905,  // df=16
	2.109816,  // df=17
	2.100922,  // df=18
	2.093024,  // df=19
	2.085963,  // df=20
	2.079614,  // df=21
	2.073873,  // df=22
	2.068658,  // df=23
	2.063899,  // df=24
	2.059539,  // df=25
	2.055529,  // df=26
	2.051831,  // df=27
	2.048407,  // df=28
	2.045230,  // df=29
	2.042272,  // df=30
}

// tCrit95 returns the two-sided 95% critical value for the confidence
// interval of a mean over n samples (Student's t with n-1 degrees of
// freedom; n < 2 produces no interval and callers gate on hasCI).
func tCrit95(n int) float64 {
	if n < 2 {
		return 0
	}
	if df := n - 1; df <= 30 {
		return tCrit95Table[df-1]
	}
	return 1.96
}

// ciDisjoint reports whether two mean±halfwidth intervals do not overlap.
// Touching intervals count as overlapping (conservative: a boundary-touching
// pair is not treated as evidence of a real difference).
func ciDisjoint(aMean, aCI, bMean, bCI float64) bool {
	return aMean+aCI < bMean-bCI || bMean+bCI < aMean-aCI
}

func sampleMean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func sampleStdDev(vs []float64) float64 {
	if len(vs) < 2 {
		return 0
	}
	// Shortcut for zero variance: identical inputs must yield exactly 0,
	// otherwise inexact binary fractions (e.g. 0.05) leave an epsilon
	// residue that renders as 9.6168e-18 in reports.
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
	sum := 0.0
	for _, v := range vs {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(vs)-1))
}

// ---------------------------------------------------------------------------
// Metric direction table
// ---------------------------------------------------------------------------

type direction int

const (
	dirNA direction = iota
	dirLowerBetter
	dirHigherBetter
)

// Direction rules. Keys are lower-cased first. "substring" entries match
// anywhere in the key; "token" entries must match a whole key segment (the
// key split on non-alphanumeric characters), so "strategy" does not match
// "rate" and "discovery" does not match "cv". Lower-better rules win when a
// key matches both families, hence "error_rate" is lower-better while
// "success_rate" is higher-better.
var (
	lowerBetterSubstrings = []string{"latency", "delay", "duration", "error", "failure", "fragment", "herd"}
	// Percentile tokens (p50/p90/p95/p99 as whole key segments) are treated
	// as lower-is-better latencies: the real-machine export blocks
	// ("create.p95", "delete.p99") carry no "latency" substring, and without
	// these tokens their verdicts would always be n/a.
	lowerBetterTokens      = []string{"cv", "p50", "p90", "p95", "p99"}
	higherBetterSubstrings = []string{"jain", "throughput"}
	higherBetterTokens     = []string{"rate", "qps"}
)

func metricDirection(key string) direction {
	k := strings.ToLower(key)
	tokens := strings.FieldsFunc(k, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if directionMatches(k, tokens, lowerBetterSubstrings, lowerBetterTokens) {
		return dirLowerBetter
	}
	if directionMatches(k, tokens, higherBetterSubstrings, higherBetterTokens) {
		return dirHigherBetter
	}
	return dirNA
}

func directionMatches(key string, tokens, substrings, exactTokens []string) bool {
	for _, s := range substrings {
		if strings.Contains(key, s) {
			return true
		}
	}
	for _, tok := range tokens {
		for _, e := range exactTokens {
			if tok == e {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Comparison model
// ---------------------------------------------------------------------------

type verdict string

const (
	verdictImproved     verdict = "improved"
	verdictRegressed    verdict = "regressed"
	verdictSame         verdict = "same"
	verdictInconclusive verdict = "inconclusive"
	verdictNoDir        verdict = "n/a"
	verdictMissing      verdict = "—" // metric absent on one side
)

type compareRow struct {
	key     string
	group   string
	base    metricStats
	hasBase bool
	cand    metricStats
	hasCand bool
	delta   float64 // candidate mean - baseline mean
	pct     float64 // delta / baseline mean * 100
	hasPct  bool    // false when baseline mean is 0 or a side is missing
	dir     direction
	verdict verdict
}

type comparison struct {
	baseline  *sampleGroup
	candidate *sampleGroup
	rows      []*compareRow // sorted by (group, key)
	generated time.Time
}

func buildComparison(baseline, candidate *sampleGroup, generated time.Time) *comparison {
	baseAgg := aggregateSamples(baseline.samples)
	candAgg := aggregateSamples(candidate.samples)

	keySet := make(map[string]struct{}, len(baseAgg)+len(candAgg))
	for k := range baseAgg {
		keySet[k] = struct{}{}
	}
	for k := range candAgg {
		keySet[k] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := groupPrefix(keys[i]), groupPrefix(keys[j])
		if gi != gj {
			return gi < gj
		}
		return keys[i] < keys[j]
	})

	cmp := &comparison{baseline: baseline, candidate: candidate, generated: generated}
	for _, k := range keys {
		row := &compareRow{key: k, group: groupPrefix(k), dir: metricDirection(k)}
		if st, ok := baseAgg[k]; ok {
			row.base, row.hasBase = st, true
		}
		if st, ok := candAgg[k]; ok {
			row.cand, row.hasCand = st, true
		}
		if row.hasBase && row.hasCand {
			row.delta = row.cand.mean - row.base.mean
			if row.base.mean != 0 {
				row.pct = row.delta / row.base.mean * 100
				row.hasPct = true
			}
		}
		row.verdict = compareVerdict(row)
		cmp.rows = append(cmp.rows, row)
	}
	return cmp
}

func compareVerdict(r *compareRow) verdict {
	switch {
	case !r.hasBase || !r.hasCand:
		return verdictMissing
	case r.dir == dirNA:
		return verdictNoDir
	case r.delta == 0:
		return verdictSame
	}
	// When both sides carry a CI, a direction verdict additionally requires
	// the two intervals to be disjoint: overlapping CIs mean the observed
	// delta is not distinguishable from sampling noise at 95% confidence,
	// and the Conclusions section lists such rows as inconclusive when |Δ%|
	// is large enough to matter. Sides without a CI (n = 1) fall back to the
	// plain mean-delta direction, matching the historical behavior of
	// single-sample runs.
	if r.base.hasCI && r.cand.hasCI && !ciDisjoint(r.base.mean, r.base.ci, r.cand.mean, r.cand.ci) {
		return verdictInconclusive
	}
	if r.delta > 0 {
		if r.dir == dirHigherBetter {
			return verdictImproved
		}
		return verdictRegressed
	}
	if r.dir == dirLowerBetter {
		return verdictImproved
	}
	return verdictRegressed
}

// groupPrefix groups metrics by their natural key prefix: the segment before
// the first '.' or '_' (e.g. "create.p95_ms" -> "create", "queue_depth" ->
// "queue"). Keys without a separator form their own group.
func groupPrefix(key string) string {
	if i := strings.IndexAny(key, "._"); i > 0 {
		return key[:i]
	}
	return key
}

// conclusions splits rows into the report's conclusion lists: significant
// improvements/regressions (verdict matches, |Δ%| >= 5%, and — once both
// sides carry a CI — disjoint intervals), the inconclusive rows that reach
// the |Δ%| threshold but fail the CI test, plus metrics without a known
// direction.
func (c *comparison) conclusions() (improved, regressed, inconclusive, noDir []*compareRow) {
	for _, r := range c.rows {
		switch {
		case r.verdict == verdictInconclusive && r.hasPct && math.Abs(r.pct) >= 5:
			inconclusive = append(inconclusive, r)
		case (r.verdict == verdictImproved || r.verdict == verdictRegressed) && r.hasPct && math.Abs(r.pct) >= 5:
			if r.verdict == verdictImproved {
				improved = append(improved, r)
			} else {
				regressed = append(regressed, r)
			}
		case r.verdict == verdictNoDir:
			noDir = append(noDir, r)
		}
	}
	byImpact := func(rows []*compareRow) {
		sort.Slice(rows, func(i, j int) bool {
			ai, aj := math.Abs(rows[i].pct), math.Abs(rows[j].pct)
			if ai != aj {
				return ai > aj
			}
			return rows[i].key < rows[j].key
		})
	}
	byImpact(improved)
	byImpact(regressed)
	byImpact(inconclusive)
	return improved, regressed, inconclusive, noDir
}

// ---------------------------------------------------------------------------
// Markdown rendering
// ---------------------------------------------------------------------------

func renderComparison(c *comparison) string {
	var b strings.Builder

	b.WriteString("# A/B Comparison Report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", c.generated.Format("2006-01-02 15:04:05 UTC"))

	b.WriteString("## Experiment Setup\n\n")
	b.WriteString("| group | name | files | samples (n) |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	fmt.Fprintf(&b, "| baseline | %s | %d | %d |\n", c.baseline.name, len(c.baseline.files), len(c.baseline.samples))
	fmt.Fprintf(&b, "| candidate | %s | %d | %d |\n", c.candidate.name, len(c.candidate.files), len(c.candidate.samples))
	b.WriteString("\n")
	writeFileList(&b, "baseline", c.baseline)
	writeFileList(&b, "candidate", c.candidate)
	if line := configHighlights("baseline", c.baseline); line != "" {
		b.WriteString(line + "\n")
	}
	if line := configHighlights("candidate", c.candidate); line != "" {
		b.WriteString(line + "\n")
	}

	b.WriteString("\n## Metric Comparison\n\n")
	b.WriteString("Cells show the mean across samples; CI is the 95% confidence interval half-width (Student's t, t(0.975, n−1)·σ/√n — wider than 1.96·σ/√n for small n), shown only when n ≥ 2. Δ = candidate − baseline. When both sides show a CI, improved/regressed additionally requires the two intervals to be disjoint.\n\n")
	if len(c.rows) == 0 {
		b.WriteString("- (no numeric metrics found)\n")
	}
	group := ""
	for _, r := range c.rows {
		if r.group != group {
			if group != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "### %s\n\n", r.group)
			b.WriteString("| metric | baseline (mean±CI) | candidate (mean±CI) | Δ | Δ% | verdict |\n")
			b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
			group = r.group
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.key, statsCell(r.base, r.hasBase), statsCell(r.cand, r.hasCand),
			deltaCell(r), pctCell(r), string(r.verdict))
	}

	improved, regressed, inconclusive, noDir := c.conclusions()
	b.WriteString("\n## Conclusions\n\n")
	b.WriteString("Only |Δ%| ≥ 5% is listed. improved/regressed additionally requires disjoint CIs when both sides carry one; rows that reach the Δ% threshold but overlap are listed as inconclusive. The CI covers sample-to-sample (file/round) variance only — trace-level variance requires multiple traces or seeds.\n\n")
	b.WriteString("### Improved (|Δ%| ≥ 5%)\n\n")
	writeConclusionList(&b, improved)
	b.WriteString("\n### Regressed (|Δ%| ≥ 5%)\n\n")
	writeConclusionList(&b, regressed)
	b.WriteString("\n### Inconclusive (|Δ%| ≥ 5%, CI overlap)\n\n")
	writeConclusionList(&b, inconclusive)
	b.WriteString("\n### No direction (n/a)\n\n")
	writeConclusionList(&b, noDir)
	b.WriteString("\n")
	return b.String()
}

func writeFileList(b *strings.Builder, label string, g *sampleGroup) {
	parts := make([]string, len(g.files))
	for i, f := range g.files {
		note := fmt.Sprintf("n=%d", len(f.samples))
		if f.viaRounds {
			note += " via rounds"
		}
		parts[i] = fmt.Sprintf("`%s` (%s)", f.path, note)
	}
	fmt.Fprintf(b, "- **%s files**: %s\n", label, strings.Join(parts, ", "))
}

// configHighlightKeys are the config fields surfaced in the report metadata
// when present. Values are de-duplicated per group, so files that differ only
// by seed render as e.g. "seed=1, 2, 3".
var configHighlightKeys = []string{"seed", "seeds", "rounds", "workload", "profile", "version", "template", "mode"}

func configHighlights(label string, g *sampleGroup) string {
	var parts []string
	for _, key := range configHighlightKeys {
		seen := make(map[string]struct{})
		var vals []string
		for _, f := range g.files {
			if f.config == nil {
				continue
			}
			v, ok := f.config[key]
			if !ok {
				continue
			}
			s := formatConfigValue(v)
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			vals = append(vals, s)
		}
		if len(vals) > 0 {
			parts = append(parts, key+"="+strings.Join(vals, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("- **%s config**: %s", label, strings.Join(parts, "; "))
}

func formatConfigValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		data, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(data)
	}
}

func writeConclusionList(b *strings.Builder, rows []*compareRow) {
	if len(rows) == 0 {
		b.WriteString("- (none)\n")
		return
	}
	for _, r := range rows {
		fmt.Fprintf(b, "- **%s**: %s (%s → %s)\n",
			r.key, pctCell(r), formatNum(r.base.mean), formatNum(r.cand.mean))
	}
}

func statsCell(st metricStats, ok bool) string {
	if !ok {
		return "—"
	}
	if !st.hasCI {
		return formatNum(st.mean)
	}
	return formatNum(st.mean) + " ± " + formatNum(st.ci)
}

func deltaCell(r *compareRow) string {
	if !r.hasBase || !r.hasCand {
		return "—"
	}
	return signedNum(r.delta)
}

func pctCell(r *compareRow) string {
	if !r.hasPct {
		return "—"
	}
	return fmt.Sprintf("%+.1f%%", r.pct)
}

// formatNum renders integral values without decimals and everything else with
// up to 5 significant digits.
func formatNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', 5, 64)
}

func signedNum(v float64) string {
	if v > 0 {
		return "+" + formatNum(v)
	}
	return formatNum(v)
}

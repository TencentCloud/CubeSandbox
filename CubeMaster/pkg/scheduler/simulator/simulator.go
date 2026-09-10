// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package simulator provides a deterministic offline scheduler benchmark.
package simulator

import (
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"sort"
	"strings"
)

const (
	ProfileDefault               = "default"
	ProfileBalancedSpread        = "balanced_spread"
	ProfileTemplateLocalityFirst = "template_locality_first"
	ProfileBinpackUtilization    = "binpack_utilization"

	WorkloadBurstShortLived     = "burst_short_lived"
	WorkloadSameTemplateRepeat  = "same_template_repeated"
	WorkloadMixedSizeCreate     = "mixed_size"
	defaultPriorityCandidateNum = 3

	// MinNodeCount and MaxNodeCount bound the simulated node set. The four
	// declared node specs are the only supported topology; reports must
	// describe exactly the count that was simulated.
	MinNodeCount = 1
	MaxNodeCount = 4

	ComparisonImproved  = "improved"
	ComparisonTradeOff  = "trade_off"
	ComparisonNeutral   = "neutral"
	ComparisonRegressed = "regressed"

	comparisonRateThreshold    = 0.005
	comparisonLatencyThreshold = 1.0
	comparisonSuccessEpsilon   = 1e-9
)

type Config struct {
	Seed      int64    `json:"seed"`
	NodeCount int      `json:"node_count"`
	Profiles  []string `json:"profiles"`
	Workloads []string `json:"workloads"`
}

type Report struct {
	RunID          string             `json:"run_id"`
	Config         Config             `json:"config"`
	AcceptancePath AcceptancePath     `json:"acceptance_path"`
	MetricSchema   []MetricSchema     `json:"metric_schema"`
	Results        []ProfileResult    `json:"results"`
	Comparisons    []ComparisonResult `json:"comparisons"`
}

type ComparisonResult struct {
	Workload         string             `json:"workload"`
	BaselineProfile  string             `json:"baseline_profile"`
	CandidateProfile string             `json:"candidate_profile"`
	Result           string             `json:"result"`
	Deltas           map[string]float64 `json:"deltas"`
	ImprovedMetrics  []string           `json:"improved_metrics"`
	RegressedMetrics []string           `json:"regressed_metrics"`
	Notes            []string           `json:"notes"`
}

type AcceptancePath struct {
	AcceptancePath   string `json:"acceptance_path"`
	DomainLens       string `json:"domain_lens"`
	FailurePath      string `json:"failure_path"`
	EvidencePath     string `json:"evidence_path"`
	ReviewPath       string `json:"review_path"`
	DistinctiveAngle string `json:"distinctive_angle"`
}

type MetricSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Direction   string `json:"direction"`
}

type ProfileResult struct {
	Profile   string           `json:"profile"`
	Workloads []WorkloadResult `json:"workloads"`
}

type WorkloadResult struct {
	Workload string  `json:"workload"`
	Metrics  Metrics `json:"metrics"`
}

type Metrics struct {
	TotalRequests           int                 `json:"total_requests"`
	ScheduledRequests       int                 `json:"scheduled_requests"`
	RejectedRequests        int                 `json:"rejected_requests"`
	SuccessRate             float64             `json:"schedule_success_rate"`
	PlacementCounts         map[string]int      `json:"placement_counts"`
	NodeLoadBalance         float64             `json:"node_load_balance"`
	TemplateLocalityHitRate float64             `json:"template_locality_hit_rate"`
	AverageCPUUtilization   float64             `json:"cpu_quota_utilization"`
	PeakCPUUtilization      float64             `json:"peak_cpu_utilization"`
	AverageMemUtilization   float64             `json:"mem_quota_utilization"`
	PeakMemUtilization      float64             `json:"peak_mem_utilization"`
	CreateLatencyP50MS      float64             `json:"create_latency_p50_ms"`
	CreateLatencyP95MS      float64             `json:"create_latency_p95_ms"`
	UsesEstimatedLatency    bool                `json:"uses_estimated_latency"`
	AverageCPUHeadroom      float64             `json:"average_cpu_headroom"`
	AverageScoreMargin      float64             `json:"average_score_margin"`
	AverageCandidatesScored float64             `json:"average_candidates_scored"`
	ScoreEvaluations        int                 `json:"score_evaluations"`
	FailureReasons          map[string]int      `json:"failure_reasons,omitempty"`
	Warnings                []string            `json:"warnings,omitempty"`
	NodeFinalState          map[string]NodeLoad `json:"node_final_state"`
}

type NodeLoad struct {
	RunningSandboxCount int     `json:"running_sandbox_count"`
	UsedCPUMilli        int64   `json:"used_cpu_milli"`
	UsedMemMB           int64   `json:"used_mem_mb"`
	CPUUtilization      float64 `json:"cpu_utilization"`
	MemUtilization      float64 `json:"mem_utilization"`
}

type NodeSpec struct {
	ID            string
	CPUMilli      int64
	MemMB         int64
	MaxSandboxes  int
	WarmTemplates map[string]bool
}

type Request struct {
	ID       string
	Arrival  int
	Lifetime int
	CPUMilli int64
	MemMB    int64
	Template string
}

type activeRequest struct {
	Request
	EndsAt int
}

type simNode struct {
	spec   NodeSpec
	active []activeRequest
}

type profileWeights struct {
	resourceHeadroom float64
	spread           float64
	templateLocality float64
	binpack          float64
}

func DefaultConfig() Config {
	return Config{
		Seed:      20260903,
		NodeCount: 4,
		Profiles: []string{
			ProfileDefault,
			ProfileBalancedSpread,
			ProfileTemplateLocalityFirst,
			ProfileBinpackUtilization,
		},
		Workloads: []string{
			WorkloadBurstShortLived,
			WorkloadSameTemplateRepeat,
			WorkloadMixedSizeCreate,
		},
	}
}

func Run(cfg Config) (Report, error) {
	cfg = normalizeConfig(cfg)
	if err := validateNodeCount(cfg.NodeCount); err != nil {
		return Report{}, err
	}
	if err := validateUniqueNames(cfg.Profiles, "profile"); err != nil {
		return Report{}, err
	}
	if err := validateUniqueNames(cfg.Workloads, "workload"); err != nil {
		return Report{}, err
	}
	nodes := defaultNodes(cfg.NodeCount)
	if len(nodes) != cfg.NodeCount {
		return Report{}, fmt.Errorf("simulated node count %d disagrees with config node_count %d", len(nodes), cfg.NodeCount)
	}
	report := Report{
		RunID:          fmt.Sprintf("scheduler-sim-seed-%d-nodes-%d", cfg.Seed, cfg.NodeCount),
		Config:         cfg,
		AcceptancePath: defaultAcceptancePath(),
		MetricSchema:   defaultMetricSchema(),
		Results:        make([]ProfileResult, 0, len(cfg.Profiles)),
	}

	for _, profile := range cfg.Profiles {
		if _, err := weightsForProfile(profile); err != nil {
			return Report{}, err
		}
		result := ProfileResult{Profile: profile}
		for _, workload := range cfg.Workloads {
			requests, err := workloadRequests(workload, cfg.Seed)
			if err != nil {
				return Report{}, err
			}
			// Copy the node set per workload so placement state does not leak.
			metrics, err := runWorkload(profile, cloneSimNodes(nodes), requests)
			if err != nil {
				return Report{}, err
			}
			result.Workloads = append(result.Workloads, WorkloadResult{
				Workload: workload,
				Metrics:  metrics,
			})
		}
		report.Results = append(report.Results, result)
	}
	report.Comparisons = buildComparisons(cfg, report.Results)
	return report, nil
}

// ValidateNodeCount reports whether count is a supported effective simulated
// node count (1 through MaxNodeCount inclusive). Zero is not valid here;
// normalizeConfig maps an unset library zero-value to the default before Run
// calls this helper. CLI callers that receive an explicit 0 should reject it
// before invoking Run.
func ValidateNodeCount(count int) error {
	return validateNodeCount(count)
}

func validateNodeCount(count int) error {
	if count < MinNodeCount || count > MaxNodeCount {
		return fmt.Errorf("unsupported node count %d: must be between %d and %d", count, MinNodeCount, MaxNodeCount)
	}
	return nil
}

func validateUniqueNames(values []string, kind string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate %s %q", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func cloneSimNodes(nodes []simNode) []simNode {
	out := make([]simNode, len(nodes))
	for i := range nodes {
		out[i] = simNode{spec: nodes[i].spec}
	}
	return out
}

// VerifyDefaultReport checks the acceptance contract of the default offline
// simulator benchmark. It validates report shape and scope, not live scheduler
// behavior or CubeAPI/Cubelet create latency.
func VerifyDefaultReport(report Report) error {
	required := DefaultConfig()
	for _, workload := range required.Workloads {
		if !containsString(report.Config.Workloads, workload) {
			return fmt.Errorf("verify default benchmark: config missing workload %q", workload)
		}
	}
	for _, profile := range required.Profiles {
		if !containsString(report.Config.Profiles, profile) {
			return fmt.Errorf("verify default benchmark: config missing profile %q", profile)
		}
	}

	schemaNames := make(map[string]bool, len(report.MetricSchema))
	for _, metric := range report.MetricSchema {
		schemaNames[metric.Name] = true
	}
	requiredMetrics := []string{
		"schedule_success_rate",
		"cpu_quota_utilization",
		"mem_quota_utilization",
		"node_load_balance",
		"template_locality_hit_rate",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
		"uses_estimated_latency",
	}
	if len(report.MetricSchema) < 5 {
		return fmt.Errorf("verify default benchmark: metric schema has %d entries, want at least 5", len(report.MetricSchema))
	}
	for _, name := range requiredMetrics {
		if !schemaNames[name] {
			return fmt.Errorf("verify default benchmark: metric schema missing %q", name)
		}
	}

	results := make(map[string]map[string]Metrics, len(report.Results))
	for _, profile := range report.Results {
		if _, exists := results[profile.Profile]; exists {
			return fmt.Errorf("verify default benchmark: duplicate profile result %q", profile.Profile)
		}
		workloads := make(map[string]Metrics, len(profile.Workloads))
		for _, workload := range profile.Workloads {
			if _, exists := workloads[workload.Workload]; exists {
				return fmt.Errorf("verify default benchmark: duplicate metrics for %s/%s", profile.Profile, workload.Workload)
			}
			workloads[workload.Workload] = workload.Metrics
		}
		results[profile.Profile] = workloads
	}
	for _, profile := range required.Profiles {
		workloads, ok := results[profile]
		if !ok {
			return fmt.Errorf("verify default benchmark: profile %q was not run", profile)
		}
		for _, workload := range required.Workloads {
			metrics, ok := workloads[workload]
			if !ok {
				return fmt.Errorf("verify default benchmark: missing metrics for %s/%s", profile, workload)
			}
			if metrics.TotalRequests <= 0 {
				return fmt.Errorf("verify default benchmark: %s/%s has no workload requests", profile, workload)
			}
			if metrics.ScheduledRequests+metrics.RejectedRequests != metrics.TotalRequests {
				return fmt.Errorf("verify default benchmark: %s/%s request accounting is inconsistent", profile, workload)
			}
			if !metrics.UsesEstimatedLatency {
				return fmt.Errorf("verify default benchmark: %s/%s uses_estimated_latency must be true", profile, workload)
			}
		}
	}

	comparisons := make(map[string]ComparisonResult, len(report.Comparisons))
	for _, comparison := range report.Comparisons {
		key := comparison.CandidateProfile + "\x00" + comparison.Workload
		if comparison.BaselineProfile == ProfileDefault {
			if _, exists := comparisons[key]; exists {
				return fmt.Errorf("verify default benchmark: duplicate default comparison for %s/%s", comparison.CandidateProfile, comparison.Workload)
			}
			comparisons[key] = comparison
		}
	}
	for _, profile := range required.Profiles {
		if profile == ProfileDefault {
			continue
		}
		for _, workload := range required.Workloads {
			comparison, ok := comparisons[profile+"\x00"+workload]
			if !ok {
				return fmt.Errorf("verify default benchmark: missing default-vs-%s comparison for workload %q", profile, workload)
			}
			if !validComparisonResult(comparison.Result) {
				return fmt.Errorf("verify default benchmark: %s/%s comparison has invalid result %q", profile, workload, comparison.Result)
			}
			for _, name := range comparisonDeltaKeys {
				if _, ok := comparison.Deltas[name]; !ok {
					return fmt.Errorf("verify default benchmark: %s/%s comparison missing delta %q", profile, workload, name)
				}
			}
		}
	}

	if !hasSimulatorOnlyEstimatedLatencyContract(report) {
		return fmt.Errorf("verify default benchmark: report contract must identify latency as simulator-only estimates, not live measurements")
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validComparisonResult(result string) bool {
	switch result {
	case ComparisonImproved, ComparisonTradeOff, ComparisonNeutral, ComparisonRegressed:
		return true
	default:
		return false
	}
}

func hasSimulatorOnlyEstimatedLatencyContract(report Report) bool {
	schemaOK := false
	for _, metric := range report.MetricSchema {
		if metric.Name != "uses_estimated_latency" {
			continue
		}
		desc := strings.ToLower(metric.Description)
		if metric.Direction != "true means estimated" {
			return false
		}
		if !(strings.Contains(desc, "simulat") &&
			strings.Contains(desc, "estimat") &&
			(strings.Contains(desc, "not live") || strings.Contains(desc, "not measured"))) {
			return false
		}
		schemaOK = true
		break
	}
	if !schemaOK {
		return false
	}
	if len(report.Comparisons) == 0 {
		return false
	}
	for _, comparison := range report.Comparisons {
		text := strings.ToLower(strings.Join(comparison.Notes, " "))
		if !(strings.Contains(text, "simulat") &&
			strings.Contains(text, "estimat") &&
			(strings.Contains(text, "not live") || strings.Contains(text, "not measured") || strings.Contains(text, "offline"))) {
			return false
		}
	}
	return true
}

func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Scheduler Simulator Benchmark\n\n")
	fmt.Fprintf(&b, "- run_id: `%s`\n", r.RunID)
	fmt.Fprintf(&b, "- seed: `%d`\n", r.Config.Seed)
	fmt.Fprintf(&b, "- node_count: `%d`\n", r.Config.NodeCount)
	fmt.Fprintf(&b, "- workloads: `%s`\n", strings.Join(r.Config.Workloads, ", "))
	fmt.Fprintf(&b, "- profiles: `%s`\n\n", strings.Join(r.Config.Profiles, ", "))

	fmt.Fprintf(&b, "## Acceptance Map\n\n")
	fmt.Fprintf(&b, "- acceptance path: %s\n", r.AcceptancePath.AcceptancePath)
	fmt.Fprintf(&b, "- domain lens: %s\n", r.AcceptancePath.DomainLens)
	fmt.Fprintf(&b, "- failure path: %s\n", r.AcceptancePath.FailurePath)
	fmt.Fprintf(&b, "- evidence path: %s\n", r.AcceptancePath.EvidencePath)
	fmt.Fprintf(&b, "- review path: %s\n", r.AcceptancePath.ReviewPath)
	fmt.Fprintf(&b, "- distinctive angle: %s\n\n", r.AcceptancePath.DistinctiveAngle)

	fmt.Fprintf(&b, "## Results\n\n")
	fmt.Fprintf(&b, "| profile | workload | success | rejected | locality hit | load balance | cpu util | mem util | latency p50 ms | latency p95 ms | avg candidates |\n")
	fmt.Fprintf(&b, "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, profile := range r.Results {
		for _, workload := range profile.Workloads {
			m := workload.Metrics
			fmt.Fprintf(&b, "| %s | %s | %.2f | %d | %.2f | %.3f | %.2f | %.2f | %.1f | %.1f | %.2f |\n",
				profile.Profile,
				workload.Workload,
				m.SuccessRate,
				m.RejectedRequests,
				m.TemplateLocalityHitRate,
				m.NodeLoadBalance,
				m.AverageCPUUtilization,
				m.AverageMemUtilization,
				m.CreateLatencyP50MS,
				m.CreateLatencyP95MS,
				m.AverageCandidatesScored)
		}
	}

	fmt.Fprintf(&b, "\n## Comparisons\n\n")
	fmt.Fprintf(&b, "| workload | candidate | result | improved metrics | regressed metrics | notes |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|\n")
	for _, cmp := range r.Comparisons {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			cmp.Workload,
			cmp.CandidateProfile,
			cmp.Result,
			joinOrDash(cmp.ImprovedMetrics),
			joinOrDash(cmp.RegressedMetrics),
			joinOrDash(cmp.Notes))
	}

	fmt.Fprintf(&b, "\n## Metric Contract\n\n")
	for _, metric := range r.MetricSchema {
		fmt.Fprintf(&b, "- `%s` (%s): %s\n", metric.Name, metric.Direction, metric.Description)
	}
	return b.String()
}

func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

var comparisonDeltaKeys = []string{
	"schedule_success_rate",
	"cpu_quota_utilization",
	"mem_quota_utilization",
	"node_load_balance",
	"template_locality_hit_rate",
	"create_latency_p50_ms",
	"create_latency_p95_ms",
}

func buildComparisons(cfg Config, results []ProfileResult) []ComparisonResult {
	byProfile := make(map[string]map[string]Metrics, len(results))
	for _, profile := range results {
		byWorkload := make(map[string]Metrics, len(profile.Workloads))
		for _, workload := range profile.Workloads {
			byWorkload[workload.Workload] = workload.Metrics
		}
		byProfile[profile.Profile] = byWorkload
	}
	baseline, ok := byProfile[ProfileDefault]
	if !ok {
		return []ComparisonResult{}
	}

	comparisons := make([]ComparisonResult, 0)
	for _, workload := range cfg.Workloads {
		baseMetrics, ok := baseline[workload]
		if !ok {
			continue
		}
		for _, profile := range cfg.Profiles {
			if profile == ProfileDefault {
				continue
			}
			candidateMetrics, ok := byProfile[profile][workload]
			if !ok {
				continue
			}
			comparisons = append(comparisons, compareAgainstBaseline(workload, profile, baseMetrics, candidateMetrics))
		}
	}
	return comparisons
}

func compareAgainstBaseline(workload, candidate string, baseline, metrics Metrics) ComparisonResult {
	deltas := map[string]float64{
		"schedule_success_rate":      metrics.SuccessRate - baseline.SuccessRate,
		"cpu_quota_utilization":      metrics.AverageCPUUtilization - baseline.AverageCPUUtilization,
		"mem_quota_utilization":      metrics.AverageMemUtilization - baseline.AverageMemUtilization,
		"node_load_balance":          metrics.NodeLoadBalance - baseline.NodeLoadBalance,
		"template_locality_hit_rate": metrics.TemplateLocalityHitRate - baseline.TemplateLocalityHitRate,
		"create_latency_p50_ms":      metrics.CreateLatencyP50MS - baseline.CreateLatencyP50MS,
		"create_latency_p95_ms":      metrics.CreateLatencyP95MS - baseline.CreateLatencyP95MS,
	}
	improved := make([]string, 0)
	regressed := make([]string, 0)
	for _, key := range comparisonDeltaKeys {
		switch classifyDelta(key, deltas[key]) {
		case ComparisonImproved:
			improved = append(improved, key)
		case ComparisonRegressed:
			regressed = append(regressed, key)
		}
	}

	successDeclined := deltas["schedule_success_rate"] < -comparisonSuccessEpsilon
	result := ComparisonNeutral
	switch {
	case successDeclined && len(improved) == 0:
		result = ComparisonRegressed
	case successDeclined:
		result = ComparisonTradeOff
	case len(improved) > 0 && len(regressed) > 0:
		result = ComparisonTradeOff
	case len(improved) > 0:
		result = ComparisonImproved
	case len(regressed) > 0:
		result = ComparisonRegressed
	}

	return ComparisonResult{
		Workload:         workload,
		BaselineProfile:  ProfileDefault,
		CandidateProfile: candidate,
		Result:           result,
		Deltas:           deltas,
		ImprovedMetrics:  improved,
		RegressedMetrics: regressed,
		Notes:            comparisonNotes(workload, candidate, result, successDeclined, improved, regressed),
	}
}

func classifyDelta(name string, delta float64) string {
	threshold := comparisonRateThreshold
	if name == "create_latency_p50_ms" || name == "create_latency_p95_ms" {
		threshold = comparisonLatencyThreshold
	}
	if math.Abs(delta) < threshold {
		return ComparisonNeutral
	}
	lowerIsBetter := name == "create_latency_p50_ms" || name == "create_latency_p95_ms"
	if lowerIsBetter {
		if delta < 0 {
			return ComparisonImproved
		}
		return ComparisonRegressed
	}
	if delta > 0 {
		return ComparisonImproved
	}
	return ComparisonRegressed
}

func comparisonNotes(workload, candidate, result string, successDeclined bool, improved, regressed []string) []string {
	notes := []string{
		fmt.Sprintf("Observed in this simulated workload %q: candidate profile %q versus baseline %q is classified as %s.", workload, candidate, ProfileDefault, result),
	}
	if successDeclined {
		notes = append(notes, "Schedule success rate declined, so the result is not treated as an overall improvement.")
	}
	if hasLatencyMetric(improved) || hasLatencyMetric(regressed) {
		notes = append(notes, "Latency metrics are estimated by the simulator, not measured from a live cluster.")
	}
	notes = append(notes, "Deltas are candidate minus baseline; latency decreases are improvements. These numbers are offline estimates, not live cluster measurements.")
	return notes
}

func hasLatencyMetric(metrics []string) bool {
	for _, m := range metrics {
		if m == "create_latency_p50_ms" || m == "create_latency_p95_ms" {
			return true
		}
	}
	return false
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}

func normalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	if cfg.Seed == 0 {
		cfg.Seed = def.Seed
	}
	if cfg.NodeCount == 0 {
		cfg.NodeCount = def.NodeCount
	}
	if len(cfg.Profiles) == 0 {
		cfg.Profiles = def.Profiles
	}
	if len(cfg.Workloads) == 0 {
		cfg.Workloads = def.Workloads
	}
	return cfg
}

func defaultAcceptancePath() AcceptancePath {
	return AcceptancePath{
		AcceptancePath:   "Each workload runs once per profile and reports baseline-vs-profile placement and quality metrics.",
		DomainLens:       "Scheduler score semantics: filter infeasible nodes first, score remaining candidates, then bind the highest score.",
		FailurePath:      "Requests that cannot fit any node are counted as rejected with explicit failure reasons; invalid profile/workload names fail the run.",
		EvidencePath:     "The JSON and Markdown reports include seed, node count, workload definitions, profile names, placement counts, and metric schema.",
		ReviewPath:       "Offline deterministic benchmark package plus thin CLI; no production scheduler default behavior changes.",
		DistinctiveAngle: "Measurement-path integrity and claim-evidence mapping are built into the generated report instead of only producing headline numbers.",
	}
}

func defaultMetricSchema() []MetricSchema {
	return []MetricSchema{
		{Name: "schedule_success_rate", Direction: "higher is better", Description: "Scheduled requests divided by total workload requests."},
		{Name: "rejected_requests", Direction: "lower is better", Description: "Requests rejected because no candidate node had enough simulated capacity."},
		{Name: "node_load_balance", Direction: "higher is better", Description: "One minus the coefficient of variation across per-node resource load, clamped to [0,1]."},
		{Name: "template_locality_hit_rate", Direction: "higher is better", Description: "Fraction of scheduled requests placed on a node that already has the request template."},
		{Name: "cpu_quota_utilization", Direction: "higher is better", Description: "Average final CPU quota utilization across nodes."},
		{Name: "mem_quota_utilization", Direction: "higher is better", Description: "Average final memory quota utilization across nodes."},
		{Name: "create_latency_p50_ms", Direction: "lower is better", Description: "Estimated create latency P50 from template locality, create pressure, and resource pressure. Not measured CubeAPI/Cubelet create time."},
		{Name: "create_latency_p95_ms", Direction: "lower is better", Description: "Estimated create latency P95 from template locality, create pressure, and resource pressure. Not measured CubeAPI/Cubelet create time."},
		{Name: "uses_estimated_latency", Direction: "true means estimated", Description: "Always true in this simulator: create_latency_p50_ms and create_latency_p95_ms are deterministic estimates, not live create latency."},
		{Name: "peak_cpu_utilization", Direction: "lower is safer", Description: "Highest final CPU utilization across nodes."},
		{Name: "average_cpu_headroom", Direction: "higher is safer", Description: "Average remaining CPU capacity after each placement."},
		{Name: "average_score_margin", Direction: "higher means clearer decisions", Description: "Mean score gap between the selected node and second-ranked candidate, averaged only over scheduled requests that had at least two scored candidates. Zero when no such decisions exist."},
		{Name: "average_candidates_scored", Direction: "lower means lower simulated decision cost", Description: "Average truncated ranked candidates considered per scheduled request after filtering and scoring."},
	}
}

func defaultNodes(count int) []simNode {
	base := []NodeSpec{
		{ID: "node-a", CPUMilli: 4000, MemMB: 8192, MaxSandboxes: 12, WarmTemplates: map[string]bool{"python-agent": true, "tiny-shell": true}},
		{ID: "node-b", CPUMilli: 4000, MemMB: 8192, MaxSandboxes: 12, WarmTemplates: map[string]bool{"python-agent": true}},
		{ID: "node-c", CPUMilli: 6000, MemMB: 12288, MaxSandboxes: 16, WarmTemplates: map[string]bool{"data-notebook": true}},
		{ID: "node-d", CPUMilli: 8000, MemMB: 16384, MaxSandboxes: 20, WarmTemplates: map[string]bool{"gpu-build": true, "data-notebook": true}},
	}
	// Precondition: count must already be in [MinNodeCount, MaxNodeCount].
	// Invalid input returns nil so Run's length check fails closed instead of
	// silently rewriting the requested node set.
	if count < MinNodeCount || count > len(base) {
		return nil
	}
	nodes := make([]simNode, 0, count)
	for i := 0; i < count; i++ {
		nodes = append(nodes, simNode{spec: base[i]})
	}
	return nodes
}

func workloadRequests(name string, seed int64) ([]Request, error) {
	rng := mrand.New(mrand.NewSource(seed + workloadSeedOffset(name)))
	switch name {
	case WorkloadBurstShortLived:
		requests := make([]Request, 0, 80)
		for i := 0; i < 80; i++ {
			requests = append(requests, Request{
				ID:       fmt.Sprintf("burst-%03d", i),
				Arrival:  i / 20,
				Lifetime: 3,
				CPUMilli: 250,
				MemMB:    512,
				Template: []string{"tiny-shell", "python-agent"}[rng.Intn(2)],
			})
		}
		return requests, nil
	case WorkloadSameTemplateRepeat:
		requests := make([]Request, 0, 48)
		for i := 0; i < 48; i++ {
			requests = append(requests, Request{
				ID:       fmt.Sprintf("repeat-%03d", i),
				Arrival:  i / 4,
				Lifetime: 8,
				CPUMilli: 500,
				MemMB:    1024,
				Template: "python-agent",
			})
		}
		return requests, nil
	case WorkloadMixedSizeCreate:
		shapes := []Request{
			{CPUMilli: 250, MemMB: 512, Template: "tiny-shell", Lifetime: 5},
			{CPUMilli: 750, MemMB: 2048, Template: "python-agent", Lifetime: 7},
			{CPUMilli: 1500, MemMB: 4096, Template: "data-notebook", Lifetime: 9},
			{CPUMilli: 2000, MemMB: 6144, Template: "gpu-build", Lifetime: 10},
		}
		requests := make([]Request, 0, 60)
		for i := 0; i < 60; i++ {
			shape := shapes[i%len(shapes)]
			shape.ID = fmt.Sprintf("mixed-%03d", i)
			shape.Arrival = i / 3
			requests = append(requests, shape)
		}
		return requests, nil
	default:
		return nil, fmt.Errorf("unknown workload %q", name)
	}
}

func workloadSeedOffset(name string) int64 {
	var offset int64
	for _, r := range name {
		offset += int64(r)
	}
	return offset
}

func weightsForProfile(profile string) (profileWeights, error) {
	switch profile {
	case ProfileDefault:
		return profileWeights{resourceHeadroom: 0.50, spread: 0.35, templateLocality: 0.10, binpack: 0.05}, nil
	case ProfileBalancedSpread:
		return profileWeights{resourceHeadroom: 0.35, spread: 0.50, templateLocality: 0.10, binpack: 0.05}, nil
	case ProfileTemplateLocalityFirst:
		return profileWeights{resourceHeadroom: 0.25, spread: 0.15, templateLocality: 0.55, binpack: 0.05}, nil
	case ProfileBinpackUtilization:
		return profileWeights{resourceHeadroom: 0.15, spread: 0.05, templateLocality: 0.10, binpack: 0.70}, nil
	default:
		return profileWeights{}, fmt.Errorf("unknown profile %q", profile)
	}
}

func runWorkload(profile string, nodes []simNode, requests []Request) (Metrics, error) {
	weights, err := weightsForProfile(profile)
	if err != nil {
		return Metrics{}, err
	}
	metrics := Metrics{
		TotalRequests:        len(requests),
		PlacementCounts:      make(map[string]int),
		FailureReasons:       make(map[string]int),
		NodeFinalState:       make(map[string]NodeLoad),
		RejectedRequests:     0,
		UsesEstimatedLatency: true,
	}
	latencySamples := make([]float64, 0, len(requests))

	requestsByArrival := append([]Request(nil), requests...)
	sort.SliceStable(requestsByArrival, func(i, j int) bool {
		if requestsByArrival[i].Arrival == requestsByArrival[j].Arrival {
			return requestsByArrival[i].ID < requestsByArrival[j].ID
		}
		return requestsByArrival[i].Arrival < requestsByArrival[j].Arrival
	})

	marginObservations := 0
	for _, req := range requestsByArrival {
		releaseCompleted(nodes, req.Arrival)
		ranked := scoreCandidates(nodes, req, weights)
		metrics.ScoreEvaluations += len(ranked)
		if len(ranked) == 0 {
			metrics.RejectedRequests++
			metrics.FailureReasons["no_feasible_node"]++
			continue
		}
		selected := ranked[0]
		nodes[selected.index].active = append(nodes[selected.index].active, activeRequest{
			Request: req,
			EndsAt:  req.Arrival + req.Lifetime,
		})
		metrics.ScheduledRequests++
		metrics.PlacementCounts[nodes[selected.index].spec.ID]++
		latencySamples = append(latencySamples, estimatedCreateLatencyMS(nodes[selected.index], req))
		if nodes[selected.index].spec.WarmTemplates[req.Template] {
			metrics.TemplateLocalityHitRate++
		}
		if len(ranked) > 1 {
			metrics.AverageScoreMargin += ranked[0].score - ranked[1].score
			marginObservations++
		}
		metrics.AverageCPUHeadroom += cpuHeadroom(nodes[selected.index])
	}

	if metrics.ScheduledRequests > 0 {
		denom := float64(metrics.ScheduledRequests)
		metrics.TemplateLocalityHitRate /= denom
		metrics.AverageCPUHeadroom /= denom
		metrics.AverageCandidatesScored = float64(metrics.ScoreEvaluations) / denom
	}
	if marginObservations > 0 {
		metrics.AverageScoreMargin /= float64(marginObservations)
	} else {
		metrics.AverageScoreMargin = 0
	}
	metrics.SuccessRate = ratio(metrics.ScheduledRequests, metrics.TotalRequests)
	metrics.NodeLoadBalance = nodeLoadBalance(nodes)
	metrics.AverageCPUUtilization, metrics.PeakCPUUtilization = cpuUtilization(nodes)
	metrics.AverageMemUtilization, metrics.PeakMemUtilization = memUtilization(nodes)
	metrics.CreateLatencyP50MS = percentile(latencySamples, 50)
	metrics.CreateLatencyP95MS = percentile(latencySamples, 95)
	for _, n := range nodes {
		usedCPU, usedMem := used(n)
		metrics.NodeFinalState[n.spec.ID] = NodeLoad{
			RunningSandboxCount: len(n.active),
			UsedCPUMilli:        usedCPU,
			UsedMemMB:           usedMem,
			CPUUtilization:      ratio64(usedCPU, n.spec.CPUMilli),
			MemUtilization:      ratio64(usedMem, n.spec.MemMB),
		}
	}
	if metrics.RejectedRequests > 0 {
		metrics.Warnings = append(metrics.Warnings, "workload pressure exceeded simulated node capacity for at least one request")
	}
	return metrics, nil
}

type scoredNode struct {
	index int
	score float64
}

func scoreCandidates(nodes []simNode, req Request, weights profileWeights) []scoredNode {
	ranked := make([]scoredNode, 0, len(nodes))
	for i := range nodes {
		if !fits(nodes[i], req) {
			continue
		}
		ranked = append(ranked, scoredNode{
			index: i,
			score: scoreNode(nodes[i], req, weights),
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return nodes[ranked[i].index].spec.ID < nodes[ranked[j].index].spec.ID
		}
		return ranked[i].score > ranked[j].score
	})
	if len(ranked) > defaultPriorityCandidateNum {
		return ranked[:defaultPriorityCandidateNum]
	}
	return ranked
}

func scoreNode(n simNode, req Request, weights profileWeights) float64 {
	cpuLeftAfter := cpuHeadroomAfter(n, req)
	memLeftAfter := memHeadroomAfter(n, req)
	resourceHeadroomScore := (cpuLeftAfter + memLeftAfter) / 2
	spreadScore := 1 - ratio(len(n.active), n.spec.MaxSandboxes)
	templateScore := 0.0
	if n.spec.WarmTemplates[req.Template] {
		templateScore = 1.0
	}
	binpackScore := 1 - resourceHeadroomScore
	return 100 * ((resourceHeadroomScore * weights.resourceHeadroom) +
		(spreadScore * weights.spread) +
		(templateScore * weights.templateLocality) +
		(binpackScore * weights.binpack))
}

// estimatedCreateLatencyMS returns a deterministic create-latency estimate in
// milliseconds. It is not CubeAPI or Cubelet create time. Sampling happens
// after bind, so active-sandbox and resource pressure already include the
// request being created. Do not use cpuHeadroomAfter/memHeadroomAfter here:
// those helpers add req on top of current occupancy and would double-count.
//
//	80
//	+ 120 if the selected node does not already have the request template
//	+ 80 * (active sandboxes / max sandboxes)
//	+ 60 * mean(CPU pressure, memory pressure)
func estimatedCreateLatencyMS(n simNode, req Request) float64 {
	latency := 80.0
	if !n.spec.WarmTemplates[req.Template] {
		latency += 120
	}
	latency += ratio(len(n.active), n.spec.MaxSandboxes) * 80
	usedCPU, usedMem := used(n)
	cpuPressure := ratio64(usedCPU, n.spec.CPUMilli)
	memPressure := ratio64(usedMem, n.spec.MemMB)
	latency += ((cpuPressure + memPressure) / 2) * 60
	return latency
}

func releaseCompleted(nodes []simNode, tick int) {
	for i := range nodes {
		active := nodes[i].active[:0]
		for _, req := range nodes[i].active {
			if req.EndsAt > tick {
				active = append(active, req)
			}
		}
		nodes[i].active = active
	}
}

func fits(n simNode, req Request) bool {
	if len(n.active) >= n.spec.MaxSandboxes {
		return false
	}
	usedCPU, usedMem := used(n)
	return usedCPU+req.CPUMilli <= n.spec.CPUMilli && usedMem+req.MemMB <= n.spec.MemMB
}

func used(n simNode) (int64, int64) {
	var cpu int64
	var mem int64
	for _, req := range n.active {
		cpu += req.CPUMilli
		mem += req.MemMB
	}
	return cpu, mem
}

func cpuHeadroom(n simNode) float64 {
	usedCPU, _ := used(n)
	return 1 - ratio64(usedCPU, n.spec.CPUMilli)
}

func cpuHeadroomAfter(n simNode, req Request) float64 {
	usedCPU, _ := used(n)
	return 1 - ratio64(usedCPU+req.CPUMilli, n.spec.CPUMilli)
}

func memHeadroomAfter(n simNode, req Request) float64 {
	_, usedMem := used(n)
	return 1 - ratio64(usedMem+req.MemMB, n.spec.MemMB)
}

func nodeLoadBalance(nodes []simNode) float64 {
	if len(nodes) == 0 {
		return 0
	}
	values := make([]float64, 0, len(nodes))
	for _, n := range nodes {
		usedCPU, usedMem := used(n)
		load := 0.5*ratio64(usedCPU, n.spec.CPUMilli) + 0.5*ratio64(usedMem, n.spec.MemMB)
		values = append(values, load)
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	if mean == 0 {
		return 1
	}
	var variance float64
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(values))
	balance := 1 - math.Sqrt(variance)/mean
	if balance < 0 {
		return 0
	}
	if balance > 1 {
		return 1
	}
	return balance
}

func cpuUtilization(nodes []simNode) (float64, float64) {
	var total float64
	var peak float64
	for _, n := range nodes {
		usedCPU, _ := used(n)
		util := ratio64(usedCPU, n.spec.CPUMilli)
		total += util
		if util > peak {
			peak = util
		}
	}
	return ratioFloat(total, float64(len(nodes))), peak
}

func memUtilization(nodes []simNode) (float64, float64) {
	var total float64
	var peak float64
	for _, n := range nodes {
		_, usedMem := used(n)
		util := ratio64(usedMem, n.spec.MemMB)
		total += util
		if util > peak {
			peak = util
		}
	}
	return ratioFloat(total, float64(len(nodes))), peak
}

func ratio(v, base int) float64 {
	if base == 0 {
		return 0
	}
	return float64(v) / float64(base)
}

func ratio64(v, base int64) float64 {
	if base == 0 {
		return 0
	}
	return float64(v) / float64(base)
}

func ratioFloat(v, base float64) float64 {
	if base == 0 {
		return 0
	}
	return v / base
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	weight := rank - float64(lo)
	return sorted[lo]*(1-weight) + sorted[hi]*weight
}

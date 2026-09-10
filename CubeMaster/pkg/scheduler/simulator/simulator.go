// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package simulator provides a deterministic offline scheduler benchmark.
package simulator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	ProfileDefault               = "default"
	ProfileBalancedSpread        = "balanced_spread"
	ProfileTemplateLocalityFirst = "template_locality_first"
	ProfileBinpackUtilization    = "binpack_utilization"

	WorkloadBurstShortLived    = "burst_short_lived"
	WorkloadSameTemplateRepeat = "same_template_repeated"
	WorkloadMixedSizeCreate    = "mixed_size"

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

	// UnknownRevision is the normalized value recorded when the caller could
	// not determine the source revision. It does not distinguish code
	// versions: two different working trees both report "unknown".
	UnknownRevision = "unknown"

	// runIDHashLength is how many hex characters of the canonical-config hash
	// are appended to the readable run_id prefix.
	runIDHashLength = 12
)

// Provenance records which source revision a report was generated from.
//
// The simulator library never shells out to Git. CLI callers resolve
// provenance, and library callers (including tests) inject fixed values so
// runs stay deterministic. GitDirty is nil when dirty state is unknown.
type Provenance struct {
	GitRevision string `json:"git_revision"`
	GitDirty    *bool  `json:"git_dirty"`
}

type Config struct {
	Seed      int64    `json:"seed"`
	NodeCount int      `json:"node_count"`
	Profiles  []string `json:"profiles"`
	Workloads []string `json:"workloads"`
	// Provenance is an input, not part of the benchmark selection, so it is
	// reported once under Report.Provenance instead of being duplicated
	// inside the serialized config.
	Provenance Provenance `json:"-"`
}

type Report struct {
	RunID        string             `json:"run_id"`
	Config       Config             `json:"config"`
	Provenance   Provenance         `json:"provenance"`
	MetricSchema []MetricSchema     `json:"metric_schema"`
	Results      []ProfileResult    `json:"results"`
	Comparisons  []ComparisonResult `json:"comparisons"`
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
	TotalRequests           int            `json:"total_requests"`
	ScheduledRequests       int            `json:"scheduled_requests"`
	RejectedRequests        int            `json:"rejected_requests"`
	SuccessRate             float64        `json:"schedule_success_rate"`
	PlacementCounts         map[string]int `json:"placement_counts"`
	NodeLoadBalance         float64        `json:"node_load_balance"`
	TemplateLocalityHitRate float64        `json:"template_locality_hit_rate"`
	AverageCPUUtilization   float64        `json:"cpu_quota_utilization"`
	PeakCPUUtilization      float64        `json:"peak_cpu_utilization"`
	AverageMemUtilization   float64        `json:"mem_quota_utilization"`
	PeakMemUtilization      float64        `json:"peak_mem_utilization"`
	CreateLatencyP50MS      float64        `json:"create_latency_p50_ms"`
	CreateLatencyP95MS      float64        `json:"create_latency_p95_ms"`
	UsesEstimatedLatency    bool           `json:"uses_estimated_latency"`
	AverageCPUHeadroom      float64        `json:"average_cpu_headroom"`
	AverageScoreMargin      float64        `json:"average_score_margin"`
	// AverageFeasibleCandidates is the mean number of feasible nodes per
	// scheduled request. It is a simulator-local candidate-breadth proxy, not
	// a measurement of scheduler work or latency. This simulator binds the
	// globally best scored feasible node (deterministic argmax), so a separate
	// post-cap retained-candidate metric would not change the selected node
	// here. Production CubeMaster may truncate with priority_select_num and
	// then score-weighted-random select; that bind rule is intentionally not
	// modeled.
	AverageFeasibleCandidates float64             `json:"average_feasible_candidates"`
	FeasibleEvaluations       int                 `json:"feasible_candidate_evaluations"`
	FailureReasons            map[string]int      `json:"failure_reasons,omitempty"`
	Warnings                  []string            `json:"warnings,omitempty"`
	NodeFinalState            map[string]NodeLoad `json:"node_final_state"`
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
	// run_id is derived after normalization and validation so defaulted
	// values, not raw caller input, define run identity.
	report := Report{
		RunID:        runID(cfg),
		Config:       cfg,
		Provenance:   cfg.Provenance,
		MetricSchema: defaultMetricSchema(),
		Results:      make([]ProfileResult, 0, len(cfg.Profiles)),
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

// verifyFloatTolerance is the absolute tolerance VerifyDefaultReport allows
// when it recomputes a derived float (a rate, an average, or a comparison
// delta) from the other values carried in the same report. Report values are
// float64 sums of small magnitudes, so 1e-9 is far above accumulated rounding
// error and far below any inconsistency worth reporting.
const verifyFloatTolerance = 1e-9

// requiredMetricSchemaNames are the metric-contract entries a default report
// must declare. Only the keys and the structural presence of a direction and a
// description are required; the wording of either is free to change.
var requiredMetricSchemaNames = []string{
	"schedule_success_rate",
	"rejected_requests",
	"node_load_balance",
	"template_locality_hit_rate",
	"cpu_quota_utilization",
	"mem_quota_utilization",
	"create_latency_p50_ms",
	"create_latency_p95_ms",
	"uses_estimated_latency",
	"average_feasible_candidates",
}

// VerifyDefaultReport checks that a report produced from the default
// configuration is structurally complete and internally consistent.
//
// It checks structure and cross-field consistency only. It does not measure or
// validate live scheduling performance, and it does not establish semantic
// equivalence to production scheduling. Verification never inspects
// human-readable text: metric descriptions and comparison notes can be
// reworded freely without breaking it. Scope statements belong in the
// documentation and CLI output, not in an automated check that would only
// match the generator's own prose against itself.
func VerifyDefaultReport(report Report) error {
	required := DefaultConfig()
	if err := verifyEffectiveConfig(report.Config, required); err != nil {
		return err
	}
	if err := verifyRunIdentity(report); err != nil {
		return err
	}
	if err := verifyMetricSchema(report.MetricSchema); err != nil {
		return err
	}
	results, err := verifyResults(report, required)
	if err != nil {
		return err
	}
	return verifyComparisons(report.Comparisons, results, required)
}

// verifyEffectiveConfig requires the exact default profile and workload sets,
// each without duplicates, plus a normalized seed and a supported node count.
func verifyEffectiveConfig(cfg, required Config) error {
	if err := validateUniqueNames(cfg.Profiles, "profile"); err != nil {
		return fmt.Errorf("verify default benchmark: config %w", err)
	}
	if err := validateUniqueNames(cfg.Workloads, "workload"); err != nil {
		return fmt.Errorf("verify default benchmark: config %w", err)
	}
	if len(cfg.Profiles) != len(required.Profiles) {
		return fmt.Errorf("verify default benchmark: config has %d profiles, want exactly the %d default profiles",
			len(cfg.Profiles), len(required.Profiles))
	}
	if len(cfg.Workloads) != len(required.Workloads) {
		return fmt.Errorf("verify default benchmark: config has %d workloads, want exactly the %d default workloads",
			len(cfg.Workloads), len(required.Workloads))
	}
	for _, profile := range required.Profiles {
		if !containsString(cfg.Profiles, profile) {
			return fmt.Errorf("verify default benchmark: config missing profile %q", profile)
		}
	}
	for _, workload := range required.Workloads {
		if !containsString(cfg.Workloads, workload) {
			return fmt.Errorf("verify default benchmark: config missing workload %q", workload)
		}
	}
	if err := validateNodeCount(cfg.NodeCount); err != nil {
		return fmt.Errorf("verify default benchmark: config %w", err)
	}
	if cfg.Seed == 0 {
		return fmt.Errorf("verify default benchmark: config seed is 0, want the normalized non-zero effective seed")
	}
	return nil
}

// verifyRunIdentity recomputes run_id from the effective config and the
// reported provenance, so a generator that forgets to include part of the
// selection or the revision in run identity fails verification.
func verifyRunIdentity(report Report) error {
	if strings.TrimSpace(report.Provenance.GitRevision) == "" {
		return fmt.Errorf("verify default benchmark: provenance git_revision is empty, want a revision or %q", UnknownRevision)
	}
	if report.Provenance.GitRevision == UnknownRevision && report.Provenance.GitDirty != nil {
		return fmt.Errorf("verify default benchmark: provenance git_dirty must be null when git_revision is %q", UnknownRevision)
	}
	cfg := report.Config
	cfg.Provenance = report.Provenance
	if want := runID(cfg); report.RunID != want {
		return fmt.Errorf("verify default benchmark: run_id %q does not identify the effective config and provenance, want %q",
			report.RunID, want)
	}
	return nil
}

func verifyMetricSchema(schema []MetricSchema) error {
	seen := make(map[string]struct{}, len(schema))
	for i, metric := range schema {
		if metric.Name == "" {
			return fmt.Errorf("verify default benchmark: metric_schema[%d] has an empty name", i)
		}
		if _, ok := seen[metric.Name]; ok {
			return fmt.Errorf("verify default benchmark: duplicate metric schema entry %q", metric.Name)
		}
		seen[metric.Name] = struct{}{}
		if strings.TrimSpace(metric.Direction) == "" {
			return fmt.Errorf("verify default benchmark: metric schema entry %q has no direction", metric.Name)
		}
		if strings.TrimSpace(metric.Description) == "" {
			return fmt.Errorf("verify default benchmark: metric schema entry %q has no description", metric.Name)
		}
	}
	for _, name := range requiredMetricSchemaNames {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("verify default benchmark: metric schema missing %q", name)
		}
	}
	// Every Metrics JSON field the generator can emit must have a schema entry
	// so the contract cannot silently drift from the report object.
	for _, name := range metricsJSONFieldNames() {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("verify default benchmark: metric schema missing emitted metrics key %q", name)
		}
	}
	return nil
}

// metricsJSONFieldNames returns every json-tagged Metrics field name, including
// omitempty fields, so schema coverage cannot drop behind the emitted object.
func metricsJSONFieldNames() []string {
	t := reflect.TypeOf(Metrics{})
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// verifyResults requires exactly one result entry for every expected
// (profile, workload) pair with no extras, and checks each metrics object for
// internal consistency. It returns the indexed results so comparisons can be
// checked against the values they claim to compare.
func verifyResults(report Report, required Config) (map[string]map[string]Metrics, error) {
	expectedNodeIDs := make(map[string]struct{}, report.Config.NodeCount)
	for _, node := range defaultNodes(report.Config.NodeCount) {
		expectedNodeIDs[node.spec.ID] = struct{}{}
	}
	results := make(map[string]map[string]Metrics, len(report.Results))
	for _, profile := range report.Results {
		if !containsString(required.Profiles, profile.Profile) {
			return nil, fmt.Errorf("verify default benchmark: unexpected profile result %q", profile.Profile)
		}
		if _, exists := results[profile.Profile]; exists {
			return nil, fmt.Errorf("verify default benchmark: duplicate profile result %q", profile.Profile)
		}
		workloads := make(map[string]Metrics, len(profile.Workloads))
		for _, workload := range profile.Workloads {
			if !containsString(required.Workloads, workload.Workload) {
				return nil, fmt.Errorf("verify default benchmark: unexpected workload result %s/%s",
					profile.Profile, workload.Workload)
			}
			if _, exists := workloads[workload.Workload]; exists {
				return nil, fmt.Errorf("verify default benchmark: duplicate metrics for %s/%s",
					profile.Profile, workload.Workload)
			}
			workloads[workload.Workload] = workload.Metrics
		}
		results[profile.Profile] = workloads
	}
	for _, profile := range required.Profiles {
		workloads, ok := results[profile]
		if !ok {
			return nil, fmt.Errorf("verify default benchmark: profile %q was not run", profile)
		}
		for _, workload := range required.Workloads {
			metrics, ok := workloads[workload]
			if !ok {
				return nil, fmt.Errorf("verify default benchmark: missing metrics for %s/%s", profile, workload)
			}
			if err := verifyMetrics(metrics, expectedNodeIDs); err != nil {
				return nil, fmt.Errorf("verify default benchmark: %s/%s %w", profile, workload, err)
			}
		}
	}
	return results, nil
}

// verifyMetrics checks one metrics object against the invariants that report
// generation or serialization could break. Errors are phrased as sentence
// fragments so callers can prefix them with the profile/workload pair.
func verifyMetrics(m Metrics, expectedNodeIDs map[string]struct{}) error {
	nodeCount := len(expectedNodeIDs)
	if m.TotalRequests <= 0 {
		return fmt.Errorf("has no workload requests")
	}
	if m.ScheduledRequests < 0 || m.RejectedRequests < 0 {
		return fmt.Errorf("has negative request counts (scheduled=%d rejected=%d)", m.ScheduledRequests, m.RejectedRequests)
	}
	if m.ScheduledRequests+m.RejectedRequests != m.TotalRequests {
		return fmt.Errorf("request accounting is inconsistent: scheduled=%d + rejected=%d != total=%d",
			m.ScheduledRequests, m.RejectedRequests, m.TotalRequests)
	}
	// Estimated-latency status is a typed boolean field, never inferred from
	// the wording of any description or note.
	if !m.UsesEstimatedLatency {
		return fmt.Errorf("uses_estimated_latency is false, want true for simulator-estimated create latency")
	}

	if len(m.NodeFinalState) != nodeCount {
		return fmt.Errorf("node_final_state covers %d nodes, want config node_count %d", len(m.NodeFinalState), nodeCount)
	}
	for _, id := range sortedKeys(m.NodeFinalState) {
		if _, ok := expectedNodeIDs[id]; !ok {
			return fmt.Errorf("node_final_state contains unexpected node %q", id)
		}
	}
	for _, id := range sortedKeys(expectedNodeIDs) {
		if _, ok := m.NodeFinalState[id]; !ok {
			return fmt.Errorf("node_final_state is missing expected node %q", id)
		}
	}
	if len(m.PlacementCounts) > nodeCount {
		return fmt.Errorf("placement_counts covers %d nodes, want at most config node_count %d", len(m.PlacementCounts), nodeCount)
	}
	placed := 0
	for _, id := range sortedKeys(m.PlacementCounts) {
		count := m.PlacementCounts[id]
		if count < 0 {
			return fmt.Errorf("placement_counts[%q] is negative (%d)", id, count)
		}
		if _, ok := m.NodeFinalState[id]; !ok {
			return fmt.Errorf("placement_counts references node %q that is absent from node_final_state", id)
		}
		placed += count
	}
	if placed != m.ScheduledRequests {
		return fmt.Errorf("placement_counts total %d does not equal scheduled_requests %d", placed, m.ScheduledRequests)
	}
	for _, id := range sortedKeys(m.NodeFinalState) {
		load := m.NodeFinalState[id]
		if load.RunningSandboxCount < 0 || load.UsedCPUMilli < 0 || load.UsedMemMB < 0 {
			return fmt.Errorf("node_final_state[%q] has negative occupancy (%+v)", id, load)
		}
		if err := verifyRatio(fmt.Sprintf("node_final_state[%q].cpu_utilization", id), load.CPUUtilization); err != nil {
			return err
		}
		if err := verifyRatio(fmt.Sprintf("node_final_state[%q].mem_utilization", id), load.MemUtilization); err != nil {
			return err
		}
	}

	failures := 0
	for _, reason := range sortedKeys(m.FailureReasons) {
		count := m.FailureReasons[reason]
		if count <= 0 {
			return fmt.Errorf("failure_reasons[%q] is not a positive count (%d)", reason, count)
		}
		failures += count
	}
	if failures != m.RejectedRequests {
		return fmt.Errorf("failure_reasons total %d does not equal rejected_requests %d", failures, m.RejectedRequests)
	}

	finite := []struct {
		name  string
		value float64
		ratio bool
	}{
		{"schedule_success_rate", m.SuccessRate, true},
		{"node_load_balance", m.NodeLoadBalance, true},
		{"template_locality_hit_rate", m.TemplateLocalityHitRate, true},
		{"cpu_quota_utilization", m.AverageCPUUtilization, true},
		{"peak_cpu_utilization", m.PeakCPUUtilization, true},
		{"mem_quota_utilization", m.AverageMemUtilization, true},
		{"peak_mem_utilization", m.PeakMemUtilization, true},
		{"create_latency_p50_ms", m.CreateLatencyP50MS, false},
		{"create_latency_p95_ms", m.CreateLatencyP95MS, false},
		{"average_cpu_headroom", m.AverageCPUHeadroom, true},
		{"average_score_margin", m.AverageScoreMargin, false},
		{"average_feasible_candidates", m.AverageFeasibleCandidates, false},
	}
	for _, metric := range finite {
		if math.IsNaN(metric.value) || math.IsInf(metric.value, 0) {
			return fmt.Errorf("%s is not finite (%v)", metric.name, metric.value)
		}
		if metric.ratio {
			if err := verifyRatio(metric.name, metric.value); err != nil {
				return err
			}
		}
	}
	if m.CreateLatencyP50MS < 0 || m.CreateLatencyP95MS < 0 {
		return fmt.Errorf("create latency percentiles are negative (p50=%v p95=%v)", m.CreateLatencyP50MS, m.CreateLatencyP95MS)
	}
	if m.CreateLatencyP95MS+verifyFloatTolerance < m.CreateLatencyP50MS {
		return fmt.Errorf("create_latency_p95_ms %v is below create_latency_p50_ms %v", m.CreateLatencyP95MS, m.CreateLatencyP50MS)
	}
	if diff := math.Abs(m.SuccessRate - ratio(m.ScheduledRequests, m.TotalRequests)); diff > verifyFloatTolerance {
		return fmt.Errorf("schedule_success_rate %v disagrees with scheduled/total by %v, want at most %v",
			m.SuccessRate, diff, verifyFloatTolerance)
	}

	if m.AverageFeasibleCandidates < 0 {
		return fmt.Errorf("average_feasible_candidates is negative (%v)", m.AverageFeasibleCandidates)
	}
	if m.FeasibleEvaluations < 0 {
		return fmt.Errorf("feasible_candidate_evaluations is negative (%d)", m.FeasibleEvaluations)
	}
	if m.ScheduledRequests == 0 {
		observed := []struct {
			name  string
			value float64
		}{
			{"template_locality_hit_rate", m.TemplateLocalityHitRate},
			{"create_latency_p50_ms", m.CreateLatencyP50MS},
			{"create_latency_p95_ms", m.CreateLatencyP95MS},
			{"average_cpu_headroom", m.AverageCPUHeadroom},
			{"average_score_margin", m.AverageScoreMargin},
			{"average_feasible_candidates", m.AverageFeasibleCandidates},
		}
		for _, metric := range observed {
			if metric.value != 0 {
				return fmt.Errorf("%s is %v with zero scheduled_requests, want 0", metric.name, metric.value)
			}
		}
		if m.FeasibleEvaluations != 0 {
			return fmt.Errorf("feasible_candidate_evaluations is non-zero with zero scheduled_requests (%d)",
				m.FeasibleEvaluations)
		}
	} else {
		if m.FeasibleEvaluations < m.ScheduledRequests {
			return fmt.Errorf("feasible_candidate_evaluations %d is below scheduled_requests %d",
				m.FeasibleEvaluations, m.ScheduledRequests)
		}
		if max := m.ScheduledRequests * nodeCount; m.FeasibleEvaluations > max {
			return fmt.Errorf("feasible_candidate_evaluations %d exceeds scheduled_requests*node_count bound %d",
				m.FeasibleEvaluations, max)
		}
		denom := float64(m.ScheduledRequests)
		if diff := math.Abs(m.AverageFeasibleCandidates - float64(m.FeasibleEvaluations)/denom); diff > verifyFloatTolerance {
			return fmt.Errorf("average_feasible_candidates %v disagrees with feasible_candidate_evaluations/scheduled_requests by %v",
				m.AverageFeasibleCandidates, diff)
		}
	}
	if m.AverageFeasibleCandidates > float64(nodeCount)+verifyFloatTolerance {
		return fmt.Errorf("average_feasible_candidates %v exceeds the simulated node count %d",
			m.AverageFeasibleCandidates, nodeCount)
	}
	return nil
}

// verifyComparisons requires one unique comparison per expected
// baseline/candidate pair, each referring to results present in the same
// report, with deltas and classifications that agree with those results.
func verifyComparisons(comparisons []ComparisonResult, results map[string]map[string]Metrics, required Config) error {
	expected := (len(required.Profiles) - 1) * len(required.Workloads)
	if len(comparisons) != expected {
		return fmt.Errorf("verify default benchmark: report has %d comparisons, want %d default-vs-candidate pairs",
			len(comparisons), expected)
	}
	seen := make(map[string]struct{}, len(comparisons))
	for i, cmp := range comparisons {
		if cmp.BaselineProfile != ProfileDefault {
			return fmt.Errorf("verify default benchmark: comparisons[%d] baseline_profile = %q, want %q",
				i, cmp.BaselineProfile, ProfileDefault)
		}
		if cmp.CandidateProfile == cmp.BaselineProfile {
			return fmt.Errorf("verify default benchmark: comparisons[%d] compares %q against itself", i, cmp.CandidateProfile)
		}
		key := cmp.CandidateProfile + "\x00" + cmp.Workload
		if _, ok := seen[key]; ok {
			return fmt.Errorf("verify default benchmark: duplicate comparison for %s/%s", cmp.CandidateProfile, cmp.Workload)
		}
		seen[key] = struct{}{}
		baseline, ok := results[cmp.BaselineProfile][cmp.Workload]
		if !ok {
			return fmt.Errorf("verify default benchmark: comparisons[%d] references missing baseline result %s/%s",
				i, cmp.BaselineProfile, cmp.Workload)
		}
		candidate, ok := results[cmp.CandidateProfile][cmp.Workload]
		if !ok {
			return fmt.Errorf("verify default benchmark: comparisons[%d] references missing candidate result %s/%s",
				i, cmp.CandidateProfile, cmp.Workload)
		}
		if !validComparisonResult(cmp.Result) {
			return fmt.Errorf("verify default benchmark: %s/%s comparison has invalid result %q",
				cmp.CandidateProfile, cmp.Workload, cmp.Result)
		}
		if len(cmp.Deltas) != len(comparisonDeltaKeys) {
			return fmt.Errorf("verify default benchmark: %s/%s comparison has %d deltas, want exactly %d",
				cmp.CandidateProfile, cmp.Workload, len(cmp.Deltas), len(comparisonDeltaKeys))
		}
		want := comparisonDeltas(baseline, candidate)
		for _, name := range comparisonDeltaKeys {
			got, ok := cmp.Deltas[name]
			if !ok {
				return fmt.Errorf("verify default benchmark: %s/%s comparison missing delta %q",
					cmp.CandidateProfile, cmp.Workload, name)
			}
			if math.IsNaN(got) || math.IsInf(got, 0) {
				return fmt.Errorf("verify default benchmark: %s/%s comparison delta %q is not finite (%v)",
					cmp.CandidateProfile, cmp.Workload, name, got)
			}
			if diff := math.Abs(got - want[name]); diff > verifyFloatTolerance {
				return fmt.Errorf("verify default benchmark: %s/%s comparison delta %q = %v, want %v from the referenced results (difference %v exceeds tolerance %v)",
					cmp.CandidateProfile, cmp.Workload, name, got, want[name], diff, verifyFloatTolerance)
			}
		}
		improved, regressed := classifyDeltas(cmp.Deltas)
		if !equalStringSlices(cmp.ImprovedMetrics, improved) {
			return fmt.Errorf("verify default benchmark: %s/%s improved_metrics = %v, want %v from its own deltas",
				cmp.CandidateProfile, cmp.Workload, cmp.ImprovedMetrics, improved)
		}
		if !equalStringSlices(cmp.RegressedMetrics, regressed) {
			return fmt.Errorf("verify default benchmark: %s/%s regressed_metrics = %v, want %v from its own deltas",
				cmp.CandidateProfile, cmp.Workload, cmp.RegressedMetrics, regressed)
		}
		successDeclined := cmp.Deltas["schedule_success_rate"] < -comparisonSuccessEpsilon
		if wantResult := classifyComparison(successDeclined, improved, regressed); cmp.Result != wantResult {
			return fmt.Errorf("verify default benchmark: %s/%s result = %q, want %q from its own deltas",
				cmp.CandidateProfile, cmp.Workload, cmp.Result, wantResult)
		}
	}
	for _, profile := range required.Profiles {
		if profile == ProfileDefault {
			continue
		}
		for _, workload := range required.Workloads {
			if _, ok := seen[profile+"\x00"+workload]; !ok {
				return fmt.Errorf("verify default benchmark: missing default-vs-%s comparison for workload %q", profile, workload)
			}
		}
	}
	return nil
}

func verifyRatio(name string, value float64) error {
	if value < 0 || value > 1 {
		return fmt.Errorf("%s %v is outside [0,1]", name, value)
	}
	return nil
}

// sortedKeys keeps verification error messages deterministic when a check has
// to walk a map-valued metric field.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validComparisonResult(result string) bool {
	switch result {
	case ComparisonImproved, ComparisonTradeOff, ComparisonNeutral, ComparisonRegressed:
		return true
	default:
		return false
	}
}

func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Scheduler Simulator Benchmark\n\n")
	fmt.Fprintf(&b, "- run_id: `%s`\n", r.RunID)
	fmt.Fprintf(&b, "- seed: `%d`\n", r.Config.Seed)
	fmt.Fprintf(&b, "- node_count: `%d`\n", r.Config.NodeCount)
	fmt.Fprintf(&b, "- workloads: `%s`\n", strings.Join(r.Config.Workloads, ", "))
	fmt.Fprintf(&b, "- profiles: `%s`\n", strings.Join(r.Config.Profiles, ", "))
	fmt.Fprintf(&b, "- git_revision: `%s`\n", r.Provenance.GitRevision)
	fmt.Fprintf(&b, "- git_dirty: `%s`\n\n", formatOptionalBool(r.Provenance.GitDirty))

	fmt.Fprintf(&b, "## Results\n\n")
	fmt.Fprintf(&b, "| profile | workload | success | rejected | locality hit | load balance | cpu util | mem util | latency p50 ms | latency p95 ms | avg feasible |\n")
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
				m.AverageFeasibleCandidates)
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
	deltas := comparisonDeltas(baseline, metrics)
	improved, regressed := classifyDeltas(deltas)
	successDeclined := deltas["schedule_success_rate"] < -comparisonSuccessEpsilon
	result := classifyComparison(successDeclined, improved, regressed)

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

// comparisonDeltas is the single definition of candidate-minus-baseline
// deltas. Verification reuses it to confirm the deltas a report carries agree
// with the result metrics that report says they compare.
func comparisonDeltas(baseline, candidate Metrics) map[string]float64 {
	return map[string]float64{
		"schedule_success_rate":      candidate.SuccessRate - baseline.SuccessRate,
		"cpu_quota_utilization":      candidate.AverageCPUUtilization - baseline.AverageCPUUtilization,
		"mem_quota_utilization":      candidate.AverageMemUtilization - baseline.AverageMemUtilization,
		"node_load_balance":          candidate.NodeLoadBalance - baseline.NodeLoadBalance,
		"template_locality_hit_rate": candidate.TemplateLocalityHitRate - baseline.TemplateLocalityHitRate,
		"create_latency_p50_ms":      candidate.CreateLatencyP50MS - baseline.CreateLatencyP50MS,
		"create_latency_p95_ms":      candidate.CreateLatencyP95MS - baseline.CreateLatencyP95MS,
	}
}

// classifyDeltas walks comparisonDeltaKeys in declaration order so both
// generation and verification produce identically ordered metric lists.
func classifyDeltas(deltas map[string]float64) (improved, regressed []string) {
	improved = make([]string, 0)
	regressed = make([]string, 0)
	for _, key := range comparisonDeltaKeys {
		switch classifyDelta(key, deltas[key]) {
		case ComparisonImproved:
			improved = append(improved, key)
		case ComparisonRegressed:
			regressed = append(regressed, key)
		}
	}
	return improved, regressed
}

func classifyComparison(successDeclined bool, improved, regressed []string) string {
	switch {
	case successDeclined && len(improved) == 0:
		return ComparisonRegressed
	case successDeclined:
		return ComparisonTradeOff
	case len(improved) > 0 && len(regressed) > 0:
		return ComparisonTradeOff
	case len(improved) > 0:
		return ComparisonImproved
	case len(regressed) > 0:
		return ComparisonRegressed
	default:
		return ComparisonNeutral
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
	cfg.Provenance.GitRevision = strings.TrimSpace(cfg.Provenance.GitRevision)
	if cfg.Provenance.GitRevision == "" {
		cfg.Provenance.GitRevision = UnknownRevision
	}
	if cfg.Provenance.GitRevision == UnknownRevision {
		cfg.Provenance.GitDirty = nil
	}
	return cfg
}

func formatOptionalBool(value *bool) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatBool(*value)
}

// runID identifies the full effective benchmark selection plus source
// provenance. It must be called with an already-normalized Config so that
// defaulted and caller-supplied values that mean the same thing produce the
// same identifier.
//
// The hashed representation is built only from ordered scalars and slices, so
// it never depends on Go map iteration order. Fields are separated by newlines
// and list items by a unit separator, so no combination of names can produce
// the canonical text of a different configuration.
func runID(cfg Config) string {
	var canonical strings.Builder
	fmt.Fprintf(&canonical, "seed=%d\n", cfg.Seed)
	fmt.Fprintf(&canonical, "node_count=%d\n", cfg.NodeCount)
	fmt.Fprintf(&canonical, "profiles=%s\n", strings.Join(cfg.Profiles, "\x1f"))
	fmt.Fprintf(&canonical, "workloads=%s\n", strings.Join(cfg.Workloads, "\x1f"))
	fmt.Fprintf(&canonical, "git_revision=%s\n", cfg.Provenance.GitRevision)
	fmt.Fprintf(&canonical, "git_dirty=%s\n", formatOptionalBool(cfg.Provenance.GitDirty))
	sum := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("scheduler-sim-seed-%d-nodes-%d-%s",
		cfg.Seed, cfg.NodeCount, hex.EncodeToString(sum[:])[:runIDHashLength])
}

func defaultMetricSchema() []MetricSchema {
	return []MetricSchema{
		{Name: "total_requests", Direction: "context only", Description: "Total workload requests considered in this profile/workload result."},
		{Name: "scheduled_requests", Direction: "higher is better", Description: "Requests that selected a feasible node and completed a simulated bind."},
		{Name: "rejected_requests", Direction: "lower is better", Description: "Requests rejected because no candidate node had enough simulated capacity."},
		{Name: "schedule_success_rate", Direction: "higher is better", Description: "Scheduled requests divided by total workload requests."},
		{Name: "placement_counts", Direction: "context only", Description: "Per-node count of successfully scheduled requests."},
		{Name: "node_load_balance", Direction: "higher is better", Description: "One minus the coefficient of variation across per-node resource load, clamped to [0,1]."},
		{Name: "template_locality_hit_rate", Direction: "higher is better", Description: "Fraction of scheduled requests placed on a node that already has the request template."},
		{Name: "cpu_quota_utilization", Direction: "higher is better", Description: "Average final CPU quota utilization across nodes."},
		{Name: "peak_cpu_utilization", Direction: "lower is safer", Description: "Highest final CPU utilization across nodes."},
		{Name: "mem_quota_utilization", Direction: "higher is better", Description: "Average final memory quota utilization across nodes."},
		{Name: "peak_mem_utilization", Direction: "lower is safer", Description: "Highest final memory utilization across nodes."},
		{Name: "create_latency_p50_ms", Direction: "lower is better", Description: "Estimated create latency P50 from template locality, create pressure, and resource pressure. Not measured CubeAPI/Cubelet create time."},
		{Name: "create_latency_p95_ms", Direction: "lower is better", Description: "Estimated create latency P95 from template locality, create pressure, and resource pressure. Not measured CubeAPI/Cubelet create time."},
		{Name: "uses_estimated_latency", Direction: "true means estimated", Description: "Always true in this simulator: create_latency_p50_ms and create_latency_p95_ms are deterministic estimates, not live create latency."},
		{Name: "average_cpu_headroom", Direction: "higher is safer", Description: "Average remaining CPU capacity after each placement."},
		{Name: "average_score_margin", Direction: "higher means clearer decisions", Description: "Simulator-local decision-gap proxy under deterministic argmax bind: mean score gap between the selected node and second-ranked candidate, averaged only over scheduled requests that had at least two scored candidates. Zero when no such decisions exist. Not a production selection metric."},
		{Name: "average_feasible_candidates", Direction: "no better/worse direction; breadth only", Description: "Average number of feasible simulated nodes per scheduled request; maximum is the simulated node count. Simulator-local candidate breadth, not scheduler CPU cost or latency."},
		{Name: "feasible_candidate_evaluations", Direction: "context only", Description: "Sum of feasible-node counts over scheduled requests."},
		{Name: "failure_reasons", Direction: "context only", Description: "Counts of rejected requests grouped by rejection reason."},
		{Name: "warnings", Direction: "context only", Description: "Optional non-fatal notes attached to a metrics object."},
		{Name: "node_final_state", Direction: "context only", Description: "Final per-node occupancy snapshot after the workload finishes."},
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
		metrics.FeasibleEvaluations += len(ranked)
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
		metrics.AverageFeasibleCandidates = float64(metrics.FeasibleEvaluations) / denom
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

// scoreCandidates returns every feasible node scored and sorted descending.
// This simulator binds ranked[0] (deterministic argmax with stable node-ID
// tie-break). Production CubeMaster may truncate to priority_select_num and
// then select with score-weighted randomness; truncating after a full sort
// would not change the selected node under this simulator's bind rule, so a
// separate retained-candidate metric is intentionally omitted.
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

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package simulator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRunCoversMinimumWorkloadsProfilesAndMetrics(t *testing.T) {
	report, err := Run(DefaultConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(report.Results) != 4 {
		t.Fatalf("len(report.Results) = %d, want 4 profiles", len(report.Results))
	}
	if len(report.MetricSchema) < 5 {
		t.Fatalf("len(report.MetricSchema) = %d, want at least 5 metrics", len(report.MetricSchema))
	}
	if report.AcceptancePath.AcceptancePath == "" ||
		report.AcceptancePath.DomainLens == "" ||
		report.AcceptancePath.FailurePath == "" ||
		report.AcceptancePath.EvidencePath == "" ||
		report.AcceptancePath.ReviewPath == "" ||
		report.AcceptancePath.DistinctiveAngle == "" {
		t.Fatalf("acceptance map must include all required paths: %+v", report.AcceptancePath)
	}

	for _, profile := range report.Results {
		if len(profile.Workloads) != 3 {
			t.Fatalf("profile %s has %d workloads, want 3", profile.Profile, len(profile.Workloads))
		}
		for _, workload := range profile.Workloads {
			m := workload.Metrics
			if m.TotalRequests == 0 {
				t.Fatalf("%s/%s has no requests", profile.Profile, workload.Workload)
			}
			if m.ScheduledRequests+m.RejectedRequests != m.TotalRequests {
				t.Fatalf("%s/%s scheduled + rejected = %d, want %d",
					profile.Profile, workload.Workload,
					m.ScheduledRequests+m.RejectedRequests, m.TotalRequests)
			}
			if m.AverageCandidatesScored <= 0 {
				t.Fatalf("%s/%s AverageCandidatesScored = %f, want > 0",
					profile.Profile, workload.Workload, m.AverageCandidatesScored)
			}
			if m.CreateLatencyP50MS <= 0 || m.CreateLatencyP95MS <= 0 {
				t.Fatalf("%s/%s latency p50/p95 = %f/%f, want > 0",
					profile.Profile, workload.Workload, m.CreateLatencyP50MS, m.CreateLatencyP95MS)
			}
			if !m.UsesEstimatedLatency {
				t.Fatalf("%s/%s UsesEstimatedLatency = false, want true", profile.Profile, workload.Workload)
			}
		}
	}
}

func TestVerifyDefaultReportPasses(t *testing.T) {
	report, err := Run(DefaultConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := VerifyDefaultReport(report); err != nil {
		t.Fatalf("VerifyDefaultReport() error = %v, want nil", err)
	}
}

func TestVerifyDefaultReportRejectsContractGaps(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Report)
		want   string
	}{
		{
			name: "missing workload",
			mutate: func(report *Report) {
				report.Results[0].Workloads = report.Results[0].Workloads[1:]
			},
			want: "missing metrics",
		},
		{
			name: "missing profile",
			mutate: func(report *Report) {
				report.Results = report.Results[1:]
			},
			want: "was not run",
		},
		{
			name: "missing metric",
			mutate: func(report *Report) {
				for i, metric := range report.MetricSchema {
					if metric.Name == "schedule_success_rate" {
						report.MetricSchema = append(report.MetricSchema[:i], report.MetricSchema[i+1:]...)
						return
					}
				}
			},
			want: "metric schema missing",
		},
		{
			name: "missing comparison",
			mutate: func(report *Report) {
				report.Comparisons = report.Comparisons[1:]
			},
			want: "missing default-vs-",
		},
		{
			name: "missing comparison metric",
			mutate: func(report *Report) {
				delete(report.Comparisons[0].Deltas, "schedule_success_rate")
			},
			want: "comparison missing delta",
		},
		{
			name: "missing estimated latency marker",
			mutate: func(report *Report) {
				report.Results[0].Workloads[0].Metrics.UsesEstimatedLatency = false
			},
			want: "uses_estimated_latency must be true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := Run(DefaultConfig())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			tt.mutate(&report)
			err = VerifyDefaultReport(report)
			if err == nil {
				t.Fatal("VerifyDefaultReport() error = nil, want contract failure")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("VerifyDefaultReport() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestProfilesChangePlacementForSameTemplateWorkload(t *testing.T) {
	report, err := Run(Config{
		Profiles:  []string{ProfileDefault, ProfileTemplateLocalityFirst},
		Workloads: []string{WorkloadSameTemplateRepeat},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	defaultMetrics := report.Results[0].Workloads[0].Metrics
	localityMetrics := report.Results[1].Workloads[0].Metrics
	if defaultMetrics.TemplateLocalityHitRate >= localityMetrics.TemplateLocalityHitRate {
		t.Fatalf("default locality hit rate = %f, want lower than locality profile %f",
			defaultMetrics.TemplateLocalityHitRate, localityMetrics.TemplateLocalityHitRate)
	}
	if equalCounts(defaultMetrics.PlacementCounts, localityMetrics.PlacementCounts) {
		t.Fatalf("placement counts should differ between default and template locality profiles: %v",
			defaultMetrics.PlacementCounts)
	}
	if localityMetrics.CreateLatencyP50MS > defaultMetrics.CreateLatencyP50MS &&
		localityMetrics.CreateLatencyP95MS > defaultMetrics.CreateLatencyP95MS {
		t.Fatalf("template_locality_first latency p50/p95 = %f/%f, want p50 or p95 <= default %f/%f",
			localityMetrics.CreateLatencyP50MS, localityMetrics.CreateLatencyP95MS,
			defaultMetrics.CreateLatencyP50MS, defaultMetrics.CreateLatencyP95MS)
	}
}

func TestBinpackProfileTradesBalanceForHeadroomAndDecisionCost(t *testing.T) {
	report, err := Run(Config{
		Profiles:  []string{ProfileBalancedSpread, ProfileBinpackUtilization},
		Workloads: []string{WorkloadMixedSizeCreate},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	spreadMetrics := report.Results[0].Workloads[0].Metrics
	binpackMetrics := report.Results[1].Workloads[0].Metrics
	if spreadMetrics.NodeLoadBalance <= binpackMetrics.NodeLoadBalance {
		t.Fatalf("spread load balance = %f, want higher than binpack load balance %f",
			spreadMetrics.NodeLoadBalance, binpackMetrics.NodeLoadBalance)
	}
	if spreadMetrics.AverageCPUHeadroom <= binpackMetrics.AverageCPUHeadroom {
		t.Fatalf("spread average cpu headroom = %f, want higher than binpack headroom %f",
			spreadMetrics.AverageCPUHeadroom, binpackMetrics.AverageCPUHeadroom)
	}
	if spreadMetrics.AverageCandidatesScored <= binpackMetrics.AverageCandidatesScored {
		t.Fatalf("spread average candidates = %f, want higher than binpack candidates %f",
			spreadMetrics.AverageCandidatesScored, binpackMetrics.AverageCandidatesScored)
	}
}

func TestRunRejectsUnknownProfileAndWorkload(t *testing.T) {
	unknownProfile := "not-a-real-profile"
	if _, err := Run(Config{Profiles: []string{unknownProfile}}); err == nil {
		t.Fatalf("Run() with unknown profile error = nil, want error")
	} else if !strings.Contains(err.Error(), unknownProfile) {
		t.Fatalf("unknown profile error = %q, want it to contain %q", err.Error(), unknownProfile)
	}

	unknownWorkload := "not-a-real-workload"
	if _, err := Run(Config{Workloads: []string{unknownWorkload}}); err == nil {
		t.Fatalf("Run() with unknown workload error = nil, want error")
	} else if !strings.Contains(err.Error(), unknownWorkload) {
		t.Fatalf("unknown workload error = %q, want it to contain %q", err.Error(), unknownWorkload)
	}
}

func TestComparisonsCoverDefaultBaselineAndRequiredDeltas(t *testing.T) {
	report, err := Run(DefaultConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantComparisons := 3 * 3
	if len(report.Comparisons) != wantComparisons {
		t.Fatalf("len(report.Comparisons) = %d, want %d (3 workloads * 3 non-default profiles)",
			len(report.Comparisons), wantComparisons)
	}

	requiredDeltaKeys := []string{
		"schedule_success_rate",
		"cpu_quota_utilization",
		"mem_quota_utilization",
		"node_load_balance",
		"template_locality_hit_rate",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
	}
	var localitySameTemplate *ComparisonResult
	for i := range report.Comparisons {
		cmp := report.Comparisons[i]
		if cmp.BaselineProfile != ProfileDefault {
			t.Fatalf("comparison[%d].baseline_profile = %q, want %q", i, cmp.BaselineProfile, ProfileDefault)
		}
		switch cmp.Result {
		case ComparisonImproved, ComparisonTradeOff, ComparisonNeutral, ComparisonRegressed:
		default:
			t.Fatalf("comparison[%d].result = %q, want one of improved/trade_off/neutral/regressed", i, cmp.Result)
		}
		for _, key := range requiredDeltaKeys {
			if _, ok := cmp.Deltas[key]; !ok {
				t.Fatalf("comparison[%d] %s/%s missing delta %q", i, cmp.Workload, cmp.CandidateProfile, key)
			}
		}
		if cmp.CandidateProfile == ProfileTemplateLocalityFirst && cmp.Workload == WorkloadSameTemplateRepeat {
			localitySameTemplate = &report.Comparisons[i]
		}
	}
	if localitySameTemplate == nil {
		t.Fatal("missing comparison for template_locality_first on same_template_repeated")
	}
	if localitySameTemplate.Result == ComparisonRegressed {
		t.Fatalf("template_locality_first on same_template_repeated result = %q, want not regressed", localitySameTemplate.Result)
	}
	improvedLocality := false
	for _, name := range localitySameTemplate.ImprovedMetrics {
		if name == "template_locality_hit_rate" || name == "create_latency_p50_ms" || name == "create_latency_p95_ms" {
			improvedLocality = true
			break
		}
	}
	if !improvedLocality {
		t.Fatalf("template_locality_first on same_template_repeated improved_metrics = %v, want locality or latency improvement",
			localitySameTemplate.ImprovedMetrics)
	}

	// If any comparison has latency in improved or regressed, notes must mention estimated.
	for i, cmp := range report.Comparisons {
		if hasLatencyInList(cmp.ImprovedMetrics) || hasLatencyInList(cmp.RegressedMetrics) {
			found := false
			for _, note := range cmp.Notes {
				if strings.Contains(strings.ToLower(note), "estimated") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("comparison[%d] %s/%s has latency in improved/regressed but notes missing 'estimated': %v",
					i, cmp.Workload, cmp.CandidateProfile, cmp.Notes)
			}
		}
	}
}

func hasLatencyInList(metrics []string) bool {
	for _, m := range metrics {
		if m == "create_latency_p50_ms" || m == "create_latency_p95_ms" {
			return true
		}
	}
	return false
}

func TestJSONIncludesEstimatedLatencyFields(t *testing.T) {
	report, err := Run(Config{
		Profiles:  []string{ProfileDefault},
		Workloads: []string{WorkloadBurstShortLived},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	raw, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"create_latency_p50_ms",
		"create_latency_p95_ms",
		"uses_estimated_latency",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("JSON() missing %q:\n%s", want, body)
		}
	}
}

func TestMarkdownIncludesAcceptanceMapAndResults(t *testing.T) {
	report, err := Run(Config{
		Profiles:  []string{ProfileDefault},
		Workloads: []string{WorkloadBurstShortLived},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	md := report.Markdown()
	for _, want := range []string{
		"## Acceptance Map",
		"## Results",
		"## Comparisons",
		"## Metric Contract",
		"burst_short_lived",
		"default",
		"latency p50 ms",
		"latency p95 ms",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
		"uses_estimated_latency",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("Markdown() missing %q:\n%s", want, md)
		}
	}
}

func TestEstimatedCreateLatencyIncreasesWithActivePressure(t *testing.T) {
	spec := NodeSpec{
		ID:           "node-a",
		CPUMilli:     4000,
		MemMB:        8192,
		MaxSandboxes: 12,
		WarmTemplates: map[string]bool{
			"python-agent": true,
		},
	}
	req := Request{
		ID:       "create-me",
		CPUMilli: 500,
		MemMB:    1024,
		Template: "python-agent",
	}

	low := simNode{
		spec: spec,
		active: []activeRequest{
			{Request: req},
		},
	}
	high := simNode{
		spec: spec,
		active: []activeRequest{
			{Request: Request{ID: "resident", CPUMilli: 1500, MemMB: 3072, Template: "python-agent"}},
			{Request: req},
		},
	}

	lowLatency := estimatedCreateLatencyMS(low, req)
	highLatency := estimatedCreateLatencyMS(high, req)
	if highLatency <= lowLatency {
		t.Fatalf("estimatedCreateLatencyMS(high pressure) = %f, want > low pressure %f",
			highLatency, lowLatency)
	}

	// Sampling is post-bind: occupancy already includes req. Pressure must use
	// used(n), not cpuHeadroomAfter/memHeadroomAfter (those add req again).
	wantLow := 80.0 + ratio(1, 12)*80 + ((ratio64(500, 4000)+ratio64(1024, 8192))/2)*60
	wantHigh := 80.0 + ratio(2, 12)*80 + ((ratio64(2000, 4000)+ratio64(4096, 8192))/2)*60
	if lowLatency != wantLow {
		t.Fatalf("estimatedCreateLatencyMS(low) = %f, want %f (post-bind occupancy, no double-count)",
			lowLatency, wantLow)
	}
	if highLatency != wantHigh {
		t.Fatalf("estimatedCreateLatencyMS(high) = %f, want %f (post-bind occupancy, no double-count)",
			highLatency, wantHigh)
	}
}

func TestPercentileWithoutSamplesIsZero(t *testing.T) {
	if got := percentile(nil, 50); got != 0 {
		t.Fatalf("percentile(nil, 50) = %f, want 0", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("percentile(nil, 95) = %f, want 0", got)
	}
	if got := percentile([]float64{}, 50); got != 0 {
		t.Fatalf("percentile(empty, 50) = %f, want 0", got)
	}
}

func TestPercentileSingleTwoElementsAndBounds(t *testing.T) {
	single := []float64{42}
	if got := percentile(single, 50); got != 42 {
		t.Fatalf("percentile([42], 50) = %f, want 42", got)
	}
	if got := percentile(single, 0); got != 42 {
		t.Fatalf("percentile([42], 0) = %f, want 42", got)
	}
	if got := percentile(single, 100); got != 42 {
		t.Fatalf("percentile([42], 100) = %f, want 42", got)
	}

	pair := []float64{10, 20}
	if got := percentile(pair, 0); got != 10 {
		t.Fatalf("percentile([10,20], 0) = %f, want 10", got)
	}
	if got := percentile(pair, 100); got != 20 {
		t.Fatalf("percentile([10,20], 100) = %f, want 20", got)
	}
	if got := percentile(pair, 50); got != 15 {
		t.Fatalf("percentile([10,20], 50) = %f, want 15", got)
	}
}

func TestDefaultReportJSONContractLocksSchema(t *testing.T) {
	report, err := Run(DefaultConfig())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	raw, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	root := unmarshalJSONObject(t, raw)
	assertJSONHasKeys(t, root, "default report top-level",
		"run_id", "config", "acceptance_path", "metric_schema", "results", "comparisons")

	config := jsonObject(t, root["config"], "config")
	gotWorkloads := jsonStringSlice(t, config["workloads"], "config.workloads")
	gotProfiles := jsonStringSlice(t, config["profiles"], "config.profiles")
	wantWorkloads := []string{
		WorkloadBurstShortLived,
		WorkloadSameTemplateRepeat,
		WorkloadMixedSizeCreate,
	}
	wantProfiles := []string{
		ProfileDefault,
		ProfileBalancedSpread,
		ProfileTemplateLocalityFirst,
		ProfileBinpackUtilization,
	}
	if !equalStrings(gotWorkloads, wantWorkloads) {
		t.Fatalf("config.workloads = %v, want source names %v", gotWorkloads, wantWorkloads)
	}
	if !equalStrings(gotProfiles, wantProfiles) {
		t.Fatalf("config.profiles = %v, want simulator-only source names %v", gotProfiles, wantProfiles)
	}

	results := jsonArray(t, root["results"], "results")
	if len(results) != len(wantProfiles) {
		t.Fatalf("len(results) = %d, want %d profiles", len(results), len(wantProfiles))
	}
	requiredMetricKeys := []string{
		"total_requests",
		"scheduled_requests",
		"rejected_requests",
		"schedule_success_rate",
		"placement_counts",
		"node_load_balance",
		"template_locality_hit_rate",
		"cpu_quota_utilization",
		"mem_quota_utilization",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
		"uses_estimated_latency",
		"node_final_state",
	}
	for i, item := range results {
		profile := jsonObject(t, item, "results[%d]", i)
		profileName, _ := profile["profile"].(string)
		workloads := jsonArray(t, profile["workloads"], "results[%d].workloads", i)
		if len(workloads) != len(wantWorkloads) {
			t.Fatalf("profile %q has %d workloads, want %d", profileName, len(workloads), len(wantWorkloads))
		}
		for j, workloadItem := range workloads {
			workload := jsonObject(t, workloadItem, "results[%d].workloads[%d]", i, j)
			workloadName, _ := workload["workload"].(string)
			metrics := jsonObject(t, workload["metrics"], "results[%d].workloads[%d].metrics", i, j)
			assertJSONHasKeys(t, metrics, profileName+"/"+workloadName+" metrics", requiredMetricKeys...)
			estimated, ok := metrics["uses_estimated_latency"].(bool)
			if !ok || !estimated {
				t.Fatalf("%s/%s uses_estimated_latency = %v, want true: create_latency_p50_ms and create_latency_p95_ms are simulator estimates, not live CubeAPI/Cubelet create latency",
					profileName, workloadName, metrics["uses_estimated_latency"])
			}
		}
	}

	requiredDeltaKeys := []string{
		"schedule_success_rate",
		"cpu_quota_utilization",
		"mem_quota_utilization",
		"node_load_balance",
		"template_locality_hit_rate",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
	}
	allowedResults := map[string]bool{
		ComparisonImproved:  true,
		ComparisonTradeOff:  true,
		ComparisonNeutral:   true,
		ComparisonRegressed: true,
	}
	comparisons := jsonArray(t, root["comparisons"], "comparisons")
	if len(comparisons) != 9 {
		t.Fatalf("len(comparisons) = %d, want 9 (3 simulator-only candidates × 3 workloads)", len(comparisons))
	}
	for i, item := range comparisons {
		cmp := jsonObject(t, item, "comparisons[%d]", i)
		label := fmt.Sprintf("comparisons[%d]", i)
		assertJSONHasKeys(t, cmp, label,
			"workload", "baseline_profile", "candidate_profile", "result", "deltas", "notes")
		baseline, _ := cmp["baseline_profile"].(string)
		candidate, _ := cmp["candidate_profile"].(string)
		result, _ := cmp["result"].(string)
		if baseline != ProfileDefault {
			t.Fatalf("comparisons[%d].baseline_profile = %q, want %q", i, baseline, ProfileDefault)
		}
		if candidate == ProfileDefault || candidate == "" {
			t.Fatalf("comparisons[%d].candidate_profile = %q, want a non-default simulator profile", i, candidate)
		}
		if !allowedResults[result] {
			t.Fatalf("comparisons[%d].result = %q, want improved/trade_off/neutral/regressed", i, result)
		}
		deltas := jsonObject(t, cmp["deltas"], "comparisons[%d].deltas", i)
		assertJSONHasKeys(t, deltas, label+".deltas", requiredDeltaKeys...)
		if !notesHaveSimulatorOnlyEstimatedLatencyScope(jsonStringSlice(t, cmp["notes"], "comparisons[%d].notes", i)) {
			t.Fatalf("comparisons[%d] notes must keep simulator-only/estimated/not-live-or-not-measured scope, got %v",
				i, cmp["notes"])
		}
	}
}

func notesHaveSimulatorOnlyEstimatedLatencyScope(notes []string) bool {
	text := strings.ToLower(strings.Join(notes, " "))
	return strings.Contains(text, "simulat") &&
		strings.Contains(text, "estimat") &&
		(strings.Contains(text, "not live") || strings.Contains(text, "not measured"))
}

func unmarshalJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	return root
}

func jsonObject(t *testing.T, v any, name string, args ...any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s type = %T, want object", formatLabel(name, args...), v)
	}
	return obj
}

func jsonArray(t *testing.T, v any, name string, args ...any) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s type = %T, want array", formatLabel(name, args...), v)
	}
	return arr
}

func jsonStringSlice(t *testing.T, v any, name string, args ...any) []string {
	t.Helper()
	arr := jsonArray(t, v, name, args...)
	out := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s[%d] type = %T, want string", formatLabel(name, args...), i, item)
		}
		out[i] = s
	}
	return out
}

func assertJSONHasKeys(t *testing.T, obj map[string]any, label string, keys ...string) {
	t.Helper()
	missing := false
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			t.Errorf("%s missing JSON key %q", label, key)
			missing = true
		}
	}
	if missing {
		t.FailNow()
	}
}

func formatLabel(name string, args ...any) string {
	if len(args) == 0 {
		return name
	}
	return fmt.Sprintf(name, args...)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if b[k] != av {
			return false
		}
	}
	return true
}

func TestNodeCountContractLibraryAndEffectiveSet(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantErr   bool
		wantNodes int
	}{
		{name: "zero-value Config defaults to four nodes", cfg: Config{}, wantNodes: 4},
		{name: "explicit one node", cfg: Config{NodeCount: 1, Profiles: []string{ProfileDefault}, Workloads: []string{WorkloadBurstShortLived}}, wantNodes: 1},
		{name: "explicit four nodes", cfg: Config{NodeCount: 4, Profiles: []string{ProfileDefault}, Workloads: []string{WorkloadBurstShortLived}}, wantNodes: 4},
		{name: "reject negative", cfg: Config{NodeCount: -1, Profiles: []string{ProfileDefault}, Workloads: []string{WorkloadBurstShortLived}}, wantErr: true},
		{name: "reject five", cfg: Config{NodeCount: 5, Profiles: []string{ProfileDefault}, Workloads: []string{WorkloadBurstShortLived}}, wantErr: true},
		{name: "reject eight", cfg: Config{NodeCount: 8, Profiles: []string{ProfileDefault}, Workloads: []string{WorkloadBurstShortLived}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := Run(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Run() error = nil, want unsupported node count")
				}
				if !strings.Contains(err.Error(), "node count") {
					t.Fatalf("Run() error = %q, want node count message", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if report.Config.NodeCount != tt.wantNodes {
				t.Fatalf("Config.NodeCount = %d, want %d", report.Config.NodeCount, tt.wantNodes)
			}
			wantID := fmt.Sprintf("scheduler-sim-seed-%d-nodes-%d", report.Config.Seed, tt.wantNodes)
			if report.RunID != wantID {
				t.Fatalf("RunID = %q, want %q", report.RunID, wantID)
			}
			for _, profile := range report.Results {
				for _, workload := range profile.Workloads {
					if len(workload.Metrics.PlacementCounts) > tt.wantNodes {
						t.Fatalf("placement_counts has %d keys, want <= %d", len(workload.Metrics.PlacementCounts), tt.wantNodes)
					}
					if len(workload.Metrics.NodeFinalState) != tt.wantNodes {
						t.Fatalf("node_final_state has %d entries, want %d", len(workload.Metrics.NodeFinalState), tt.wantNodes)
					}
				}
			}
		})
	}
}

func TestAverageScoreMarginDenominatorIgnoresSingleCandidate(t *testing.T) {
	newNodes := func() []simNode {
		return []simNode{
			{spec: NodeSpec{ID: "small", CPUMilli: 1000, MemMB: 2048, MaxSandboxes: 8, WarmTemplates: map[string]bool{}}},
			{spec: NodeSpec{ID: "large", CPUMilli: 8000, MemMB: 16384, MaxSandboxes: 20, WarmTemplates: map[string]bool{}}},
		}
	}
	multiRequests := []Request{
		{ID: "multi-a", Arrival: 0, Lifetime: 1, CPUMilli: 100, MemMB: 100, Template: "t"},
		{ID: "multi-b", Arrival: 1, Lifetime: 1, CPUMilli: 100, MemMB: 100, Template: "t"},
	}
	singleRequests := []Request{
		{ID: "single-a", Arrival: 2, Lifetime: 1, CPUMilli: 7000, MemMB: 100, Template: "t"},
		{ID: "single-b", Arrival: 3, Lifetime: 1, CPUMilli: 7000, MemMB: 100, Template: "t"},
	}
	mixed := append(append([]Request(nil), multiRequests...), singleRequests...)

	multiOnly := runWorkload(ProfileDefault, newNodes(), multiRequests)
	mixedMetrics := runWorkload(ProfileDefault, newNodes(), mixed)
	if multiOnly.ScheduledRequests != 2 || mixedMetrics.ScheduledRequests != 4 {
		t.Fatalf("scheduled multi=%d mixed=%d, want 2 and 4", multiOnly.ScheduledRequests, mixedMetrics.ScheduledRequests)
	}
	if multiOnly.AverageScoreMargin <= 0 {
		t.Fatalf("multi-candidate AverageScoreMargin = %v, want > 0", multiOnly.AverageScoreMargin)
	}
	if mixedMetrics.AverageScoreMargin != multiOnly.AverageScoreMargin {
		t.Fatalf("mixed AverageScoreMargin = %v, want %v (single-candidate placements must not change the denominator)",
			mixedMetrics.AverageScoreMargin, multiOnly.AverageScoreMargin)
	}
	diluted := multiOnly.AverageScoreMargin * 2 / 4
	if mixedMetrics.AverageScoreMargin == diluted {
		t.Fatalf("AverageScoreMargin unexpectedly equals diluted value %v", diluted)
	}

	onlySingle := runWorkload(ProfileDefault, []simNode{
		{spec: NodeSpec{ID: "large", CPUMilli: 8000, MemMB: 16384, MaxSandboxes: 20, WarmTemplates: map[string]bool{}}},
	}, []Request{
		{ID: "only", Arrival: 0, Lifetime: 1, CPUMilli: 100, MemMB: 100, Template: "t"},
	})
	if onlySingle.AverageScoreMargin != 0 {
		t.Fatalf("single-candidate AverageScoreMargin = %v, want 0", onlySingle.AverageScoreMargin)
	}
}

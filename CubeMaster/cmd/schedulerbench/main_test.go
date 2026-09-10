// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/simulator"
)

func TestDefaultFormatIsBoth(t *testing.T) {
	if defaultFormat != formatBoth {
		t.Fatalf("default format = %q, want %q so both report.json and report.md are written", defaultFormat, formatBoth)
	}
}

func TestResolveProvenancePriorityAndFallbacks(t *testing.T) {
	t.Parallel()

	buildInfo := func(revision, modified string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modified},
			}}, true
		}
	}
	unavailableBuildInfo := func() (*debug.BuildInfo, bool) { return nil, false }

	tests := []struct {
		name          string
		readBuildInfo func() (*debug.BuildInfo, bool)
		runGit        gitCommandRunner
		wantRevision  string
		wantDirty     *bool
		wantGitCalls  int
	}{
		{
			name:          "build info success takes precedence",
			readBuildInfo: buildInfo("build-revision", "true"),
			runGit: func(context.Context, ...string) ([]byte, error) {
				return nil, errors.New("must not run")
			},
			wantRevision: "build-revision",
			wantDirty:    boolPointer(true),
		},
		{
			name:          "git fallback clean",
			readBuildInfo: unavailableBuildInfo,
			runGit: sequenceGitRunner([]gitResult{
				{output: "clean-revision\n"},
				{output: ""},
			}),
			wantRevision: "clean-revision",
			wantDirty:    boolPointer(false),
			wantGitCalls: 2,
		},
		{
			name:          "git fallback dirty",
			readBuildInfo: unavailableBuildInfo,
			runGit: sequenceGitRunner([]gitResult{
				{output: "dirty-revision\n"},
				{output: " M tracked.go\n?? new.txt\n"},
			}),
			wantRevision: "dirty-revision",
			wantDirty:    boolPointer(true),
			wantGitCalls: 2,
		},
		{
			name:          "both sources unavailable",
			readBuildInfo: unavailableBuildInfo,
			runGit: func(context.Context, ...string) ([]byte, error) {
				return nil, errors.New("git unavailable")
			},
			wantRevision: "",
			wantDirty:    nil,
			wantGitCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			runGit := func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				return tt.runGit(ctx, args...)
			}
			got := resolveProvenance(tt.readBuildInfo, runGit)
			if got.GitRevision != tt.wantRevision {
				t.Fatalf("GitRevision = %q, want %q", got.GitRevision, tt.wantRevision)
			}
			if !equalOptionalBool(got.GitDirty, tt.wantDirty) {
				t.Fatalf("GitDirty = %v, want %v", optionalBoolValue(got.GitDirty), optionalBoolValue(tt.wantDirty))
			}
			if calls != tt.wantGitCalls {
				t.Fatalf("Git calls = %d, want %d", calls, tt.wantGitCalls)
			}
		})
	}
}

func TestRunCLIUsesInjectedProvenanceResolver(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	clean := false
	resolve := func() simulator.Provenance {
		return simulator.Provenance{GitRevision: "injected-revision", GitDirty: &clean}
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLIWithProvenance([]string{
		"--out", outDir,
		"--profiles", simulator.ProfileDefault,
		"--workloads", simulator.WorkloadBurstShortLived,
	}, &stdout, &stderr, resolve); err != nil {
		t.Fatalf("runCLIWithProvenance() error = %v, stderr = %q", err, stderr.String())
	}
	report := loadCLIReportJSON(t, outDir)
	if report.Provenance.GitRevision != "injected-revision" ||
		!equalOptionalBool(report.Provenance.GitDirty, &clean) {
		t.Fatalf("report provenance = %+v, want injected clean revision", report.Provenance)
	}
}

type gitResult struct {
	output string
	err    error
}

func sequenceGitRunner(results []gitResult) gitCommandRunner {
	index := 0
	return func(context.Context, ...string) ([]byte, error) {
		if index >= len(results) {
			return nil, errors.New("unexpected Git call")
		}
		result := results[index]
		index++
		return []byte(result.output), result.err
	}
}

func boolPointer(value bool) *bool { return &value }

func equalOptionalBool(a, b *bool) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func optionalBoolValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func TestRunCLIVerifySucceeds(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "verified-report")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"--verify", "--out", outDir}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI(--verify) error = %v, stderr = %q", err, stderr.String())
	}
	if !fileExists(t, filepath.Join(outDir, "report.json")) {
		t.Fatal("runCLI(--verify) did not write report.json")
	}
	if !fileExists(t, filepath.Join(outDir, "report.md")) {
		t.Fatal("runCLI(--verify) did not write report.md")
	}
	if !bytes.Contains(stdout.Bytes(), []byte(verifyScope+" passed")) {
		t.Fatalf("runCLI(--verify) stdout = %q, want verification success", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("structure and internal consistency only")) {
		t.Fatalf("runCLI(--verify) stdout = %q, want structural/internal-consistency framing", stdout.String())
	}
}

func TestRunCLIVerifyHelpDescribesDefaultSelectionScope(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI(--help) error = %v", err)
	}
	help := stderr.String()
	for _, want := range []string{
		verifyScope,
		"not arbitrary --profiles/--workloads subsets",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("runCLI(--help) stderr = %q, want it to contain %q", help, want)
		}
	}
}

func TestRunCLIDefaultReportJSONContract(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "contract-report")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"--out", outDir}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v, stderr = %q", err, stderr.String())
	}

	raw, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}

	for _, key := range []string{"run_id", "config", "provenance", "metric_schema", "results", "comparisons"} {
		if _, ok := root[key]; !ok {
			t.Fatalf("CLI report.json missing top-level key %q", key)
		}
	}
	provenance, _ := root["provenance"].(map[string]any)
	if provenance == nil {
		t.Fatal("CLI report.json provenance is not an object")
	}
	if revision, ok := provenance["git_revision"].(string); !ok || revision == "" {
		t.Fatalf("CLI provenance.git_revision = %v, want non-empty string", provenance["git_revision"])
	}
	if _, ok := provenance["git_dirty"]; !ok {
		t.Fatal("CLI provenance missing git_dirty")
	}

	config, _ := root["config"].(map[string]any)
	if config == nil {
		t.Fatal("CLI report.json config is not an object")
	}
	if got := jsonStrings(t, config["workloads"]); !equalStrings(got, []string{
		simulator.WorkloadBurstShortLived,
		simulator.WorkloadSameTemplateRepeat,
		simulator.WorkloadMixedSizeCreate,
	}) {
		t.Fatalf("CLI config.workloads = %v, want source names", got)
	}
	if got := jsonStrings(t, config["profiles"]); !equalStrings(got, []string{
		simulator.ProfileDefault,
		simulator.ProfileBalancedSpread,
		simulator.ProfileTemplateLocalityFirst,
		simulator.ProfileBinpackUtilization,
	}) {
		t.Fatalf("CLI config.profiles = %v, want simulator-only source names", got)
	}

	results, _ := root["results"].([]any)
	requiredMetrics := []string{
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
		"average_feasible_candidates",
		"average_ranked_candidates_retained",
		"feasible_candidate_evaluations",
		"ranked_candidates_retained",
		"node_final_state",
	}
	for _, item := range results {
		profile, _ := item.(map[string]any)
		profileName, _ := profile["profile"].(string)
		workloads, _ := profile["workloads"].([]any)
		for _, workloadItem := range workloads {
			workload, _ := workloadItem.(map[string]any)
			workloadName, _ := workload["workload"].(string)
			metrics, _ := workload["metrics"].(map[string]any)
			if metrics == nil {
				t.Fatalf("CLI %s/%s metrics is not an object", profileName, workloadName)
			}
			for _, key := range requiredMetrics {
				if _, ok := metrics[key]; !ok {
					t.Fatalf("CLI %s/%s metrics missing %q", profileName, workloadName, key)
				}
			}
			for _, oldKey := range []string{"average_candidates_scored", "score_evaluations"} {
				if _, ok := metrics[oldKey]; ok {
					t.Fatalf("CLI %s/%s metrics retains removed key %q", profileName, workloadName, oldKey)
				}
			}
			estimated, ok := metrics["uses_estimated_latency"].(bool)
			if !ok || !estimated {
				t.Fatalf("CLI %s/%s uses_estimated_latency = %v, want true: create_latency_p50_ms and create_latency_p95_ms are simulator estimates, not live CubeAPI/Cubelet create latency",
					profileName, workloadName, metrics["uses_estimated_latency"])
			}
		}
	}

	allowedResults := map[string]bool{
		simulator.ComparisonImproved:  true,
		simulator.ComparisonTradeOff:  true,
		simulator.ComparisonNeutral:   true,
		simulator.ComparisonRegressed: true,
	}
	requiredDeltas := []string{
		"schedule_success_rate",
		"cpu_quota_utilization",
		"mem_quota_utilization",
		"node_load_balance",
		"template_locality_hit_rate",
		"create_latency_p50_ms",
		"create_latency_p95_ms",
	}
	comparisons, _ := root["comparisons"].([]any)
	if len(comparisons) != 9 {
		t.Fatalf("CLI len(comparisons) = %d, want 9", len(comparisons))
	}
	for i, item := range comparisons {
		cmp, _ := item.(map[string]any)
		baseline, _ := cmp["baseline_profile"].(string)
		candidate, _ := cmp["candidate_profile"].(string)
		result, _ := cmp["result"].(string)
		if baseline != simulator.ProfileDefault {
			t.Fatalf("CLI comparisons[%d].baseline_profile = %q, want default", i, baseline)
		}
		if candidate == simulator.ProfileDefault {
			t.Fatalf("CLI comparisons[%d].candidate_profile = default, want a non-default simulator profile", i)
		}
		if !allowedResults[result] {
			t.Fatalf("CLI comparisons[%d].result = %q, want improved/trade_off/neutral/regressed", i, result)
		}
		deltas, _ := cmp["deltas"].(map[string]any)
		for _, key := range requiredDeltas {
			if _, ok := deltas[key]; !ok {
				t.Fatalf("CLI comparisons[%d].deltas missing %q", i, key)
			}
		}
	}
}

func jsonStrings(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("JSON value type = %T, want string array", v)
	}
	out := make([]string, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("JSON array[%d] type = %T, want string", i, item)
		}
		out[i] = s
	}
	return out
}

func TestRunCLIVerifyFailureDoesNotWriteReport(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "invalid-report")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCLI([]string{"--verify", "--profiles", simulator.ProfileDefault, "--out", outDir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runCLI(--verify with incomplete defaults) error = nil, want verification failure")
	}
	for _, want := range []string{
		verifyScope,
		"use the default --profiles and --workloads values",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runCLI(--verify with incomplete defaults) error = %q, want it to contain %q", err, want)
		}
	}
	if fileExists(t, outDir) {
		t.Fatalf("output directory %s exists after verification failure, want no report written", outDir)
	}
}

func TestRunCLISelectsLegalProfileAndWorkloadSubsets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		args          []string
		wantProfiles  []string
		wantWorkloads []string
	}{
		{
			name: "profiles subset",
			args: []string{"--profiles", "default,balanced_spread,template_locality_first"},
			wantProfiles: []string{
				"default",
				"balanced_spread",
				"template_locality_first",
			},
			wantWorkloads: []string{
				"burst_short_lived",
				"same_template_repeated",
				"mixed_size",
			},
		},
		{
			name: "workloads subset",
			args: []string{"--workloads", "burst_short_lived,same_template_repeated"},
			wantProfiles: []string{
				"default",
				"balanced_spread",
				"template_locality_first",
				"binpack_utilization",
			},
			wantWorkloads: []string{
				"burst_short_lived",
				"same_template_repeated",
			},
		},
		{
			name: "profiles and workloads subset",
			args: []string{
				"--profiles", "default,binpack_utilization",
				"--workloads", "mixed_size",
			},
			wantProfiles: []string{
				"default",
				"binpack_utilization",
			},
			wantWorkloads: []string{
				"mixed_size",
			},
		},
		{
			name: "non-default profile without default baseline",
			args: []string{
				"--profiles", "template_locality_first",
				"--workloads", "burst_short_lived",
			},
			wantProfiles: []string{
				"template_locality_first",
			},
			wantWorkloads: []string{
				"burst_short_lived",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outDir := filepath.Join(t.TempDir(), "subset-report")
			args := append([]string{"--out", outDir}, tc.args...)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if err := runCLI(args, &stdout, &stderr); err != nil {
				t.Fatalf("runCLI(%v) error = %v, stderr = %q", tc.args, err, stderr.String())
			}

			report := loadCLIReportJSON(t, outDir)
			assertCLIReportMatchesSubset(t, report, tc.wantProfiles, tc.wantWorkloads)
		})
	}
}

func TestRunCLIRejectsUnknownProfileAndWorkloadWithoutWritingReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		illegal string
	}{
		{
			name:    "unknown profile",
			args:    []string{"--profiles", "not-a-real-profile"},
			illegal: "not-a-real-profile",
		},
		{
			name:    "unknown profile mixed with legal names",
			args:    []string{"--profiles", "default,not-a-real-profile"},
			illegal: "not-a-real-profile",
		},
		{
			name:    "unknown workload",
			args:    []string{"--workloads", "not-a-real-workload"},
			illegal: "not-a-real-workload",
		},
		{
			name:    "unknown workload mixed with legal names",
			args:    []string{"--workloads", "burst_short_lived,not-a-real-workload"},
			illegal: "not-a-real-workload",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outDir := filepath.Join(t.TempDir(), "invalid-report")
			args := append([]string{"--out", outDir}, tc.args...)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := runCLI(args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("runCLI(%v) error = nil, want unknown-name failure", tc.args)
			}
			if !strings.Contains(err.Error(), tc.illegal) {
				t.Fatalf("runCLI(%v) error = %q, want it to contain %q", tc.args, err.Error(), tc.illegal)
			}
			if fileExists(t, outDir) {
				t.Fatalf("output directory %s exists after unknown-name failure, want no report written", outDir)
			}
			if fileExists(t, filepath.Join(outDir, "report.json")) {
				t.Fatalf("report.json exists after unknown-name failure, want no report written")
			}
			if fileExists(t, filepath.Join(outDir, "report.md")) {
				t.Fatalf("report.md exists after unknown-name failure, want no report written")
			}
		})
	}
}

func TestRunCLIPreservesFormatSelection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		format       string
		wantJSON     bool
		wantMarkdown bool
	}{
		{format: formatBoth, wantJSON: true, wantMarkdown: true},
		{format: formatJSON, wantJSON: true, wantMarkdown: false},
		{format: formatMarkdown, wantJSON: false, wantMarkdown: true},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if err := runCLI([]string{"--format", tc.format, "--out", outDir}, &stdout, &stderr); err != nil {
				t.Fatalf("runCLI(--format %s) error = %v, stderr = %q", tc.format, err, stderr.String())
			}
			if got := fileExists(t, filepath.Join(outDir, "report.json")); got != tc.wantJSON {
				t.Fatalf("report.json exists = %v, want %v", got, tc.wantJSON)
			}
			if got := fileExists(t, filepath.Join(outDir, "report.md")); got != tc.wantMarkdown {
				t.Fatalf("report.md exists = %v, want %v", got, tc.wantMarkdown)
			}
		})
	}
}

func TestValidateFormat(t *testing.T) {
	t.Parallel()

	valid := []string{formatJSON, formatMarkdown, formatBoth}
	for _, format := range valid {
		if err := validateFormat(format); err != nil {
			t.Fatalf("validateFormat(%q) = %v, want nil", format, err)
		}
	}

	invalid := []string{"", "xml", "JSON", "md", "html"}
	for _, format := range invalid {
		err := validateFormat(format)
		if err == nil {
			t.Fatalf("validateFormat(%q) = nil, want error", format)
		}
	}
}

func TestWriteReportsSelectsFilesByFormat(t *testing.T) {
	t.Parallel()

	report := simulator.Report{RunID: "test-run"}
	cases := []struct {
		format       string
		wantJSON     bool
		wantMarkdown bool
	}{
		{format: formatBoth, wantJSON: true, wantMarkdown: true},
		{format: formatJSON, wantJSON: true, wantMarkdown: false},
		{format: formatMarkdown, wantJSON: false, wantMarkdown: true},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			if err := writeReports(outDir, tc.format, report); err != nil {
				t.Fatalf("writeReports(%q): %v", tc.format, err)
			}

			jsonPath := filepath.Join(outDir, "report.json")
			mdPath := filepath.Join(outDir, "report.md")
			if got := fileExists(t, jsonPath); got != tc.wantJSON {
				t.Fatalf("report.json exists = %v, want %v", got, tc.wantJSON)
			}
			if got := fileExists(t, mdPath); got != tc.wantMarkdown {
				t.Fatalf("report.md exists = %v, want %v", got, tc.wantMarkdown)
			}
		})
	}
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

func loadCLIReportJSON(t *testing.T, outDir string) simulator.Report {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var report simulator.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal report.json: %v", err)
	}
	return report
}

func assertCLIReportMatchesSubset(t *testing.T, report simulator.Report, wantProfiles, wantWorkloads []string) {
	t.Helper()
	if !equalStrings(report.Config.Profiles, wantProfiles) {
		t.Fatalf("config.profiles = %v, want %v", report.Config.Profiles, wantProfiles)
	}
	if !equalStrings(report.Config.Workloads, wantWorkloads) {
		t.Fatalf("config.workloads = %v, want %v", report.Config.Workloads, wantWorkloads)
	}
	if len(report.Results) != len(wantProfiles) {
		t.Fatalf("len(results) = %d, want %d profiles", len(report.Results), len(wantProfiles))
	}

	seenProfiles := make(map[string]bool, len(report.Results))
	for i, result := range report.Results {
		if result.Profile != wantProfiles[i] {
			t.Fatalf("results[%d].profile = %q, want %q", i, result.Profile, wantProfiles[i])
		}
		if seenProfiles[result.Profile] {
			t.Fatalf("results contain duplicate profile %q", result.Profile)
		}
		if !containsString(wantProfiles, result.Profile) {
			t.Fatalf("results contain unexpected profile %q", result.Profile)
		}
		seenProfiles[result.Profile] = true
		if len(result.Workloads) != len(wantWorkloads) {
			t.Fatalf("profile %s has %d workloads, want %d", result.Profile, len(result.Workloads), len(wantWorkloads))
		}
		seenWorkloads := make(map[string]bool, len(result.Workloads))
		for j, workload := range result.Workloads {
			if workload.Workload != wantWorkloads[j] {
				t.Fatalf("profile %s workloads[%d] = %q, want %q", result.Profile, j, workload.Workload, wantWorkloads[j])
			}
			if seenWorkloads[workload.Workload] {
				t.Fatalf("profile %s has duplicate workload %q", result.Profile, workload.Workload)
			}
			if !containsString(wantWorkloads, workload.Workload) {
				t.Fatalf("profile %s has unexpected workload %q", result.Profile, workload.Workload)
			}
			seenWorkloads[workload.Workload] = true
		}
		for _, want := range wantWorkloads {
			if !seenWorkloads[want] {
				t.Fatalf("profile %s missing workload %q", result.Profile, want)
			}
		}
	}
	for _, want := range wantProfiles {
		if !seenProfiles[want] {
			t.Fatalf("results missing profile %q", want)
		}
	}

	wantCandidates := make([]string, 0, len(wantProfiles))
	hasDefault := false
	for _, profile := range wantProfiles {
		if profile == "default" {
			hasDefault = true
			continue
		}
		wantCandidates = append(wantCandidates, profile)
	}
	wantComparisonCount := 0
	if hasDefault {
		wantComparisonCount = len(wantCandidates) * len(wantWorkloads)
	}
	if len(report.Comparisons) != wantComparisonCount {
		t.Fatalf("len(comparisons) = %d, want %d for candidates %v and workloads %v",
			len(report.Comparisons), wantComparisonCount, wantCandidates, wantWorkloads)
	}

	seenPairs := make(map[string]bool, len(report.Comparisons))
	for _, cmp := range report.Comparisons {
		if cmp.BaselineProfile != "default" {
			t.Fatalf("comparison baseline_profile = %q, want %q", cmp.BaselineProfile, "default")
		}
		if cmp.CandidateProfile == "default" {
			t.Fatalf("comparisons must not include default as a candidate, got %+v", cmp)
		}
		if !containsString(wantCandidates, cmp.CandidateProfile) {
			t.Fatalf("comparison candidate_profile = %q, want one of %v", cmp.CandidateProfile, wantCandidates)
		}
		if !containsString(wantWorkloads, cmp.Workload) {
			t.Fatalf("comparison workload = %q, want one of %v", cmp.Workload, wantWorkloads)
		}
		key := cmp.CandidateProfile + "\x00" + cmp.Workload
		if seenPairs[key] {
			t.Fatalf("duplicate comparison for %s/%s", cmp.CandidateProfile, cmp.Workload)
		}
		seenPairs[key] = true
	}
	if hasDefault {
		for _, candidate := range wantCandidates {
			for _, workload := range wantWorkloads {
				if !seenPairs[candidate+"\x00"+workload] {
					t.Fatalf("missing comparison for candidate %q workload %q", candidate, workload)
				}
			}
		}
	}
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestRunCLINodeCountBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		nodes   string
		wantErr bool
		want    int
	}{
		{name: "reject negative", nodes: "-1", wantErr: true},
		{name: "reject zero", nodes: "0", wantErr: true},
		{name: "accept one", nodes: "1", want: 1},
		{name: "accept four", nodes: "4", want: 4},
		{name: "reject five", nodes: "5", wantErr: true},
		{name: "reject eight", nodes: "8", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outDir := filepath.Join(t.TempDir(), "nodes-report")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := runCLI([]string{
				"--out", outDir,
				"--nodes", tt.nodes,
				"--profiles", simulator.ProfileDefault,
				"--workloads", simulator.WorkloadBurstShortLived,
			}, &stdout, &stderr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("runCLI() error = nil, want invalid --nodes")
				}
				if !strings.Contains(err.Error(), "nodes") {
					t.Fatalf("runCLI() error = %q, want nodes message", err)
				}
				if fileExists(t, outDir) {
					t.Fatal("report written despite invalid --nodes")
				}
				return
			}
			if err != nil {
				t.Fatalf("runCLI() error = %v, stderr = %q", err, stderr.String())
			}
			report := loadCLIReportJSON(t, outDir)
			if report.Config.NodeCount != tt.want {
				t.Fatalf("Config.NodeCount = %d, want %d", report.Config.NodeCount, tt.want)
			}
			if len(report.Results[0].Workloads[0].Metrics.NodeFinalState) != tt.want {
				t.Fatalf("node_final_state size = %d, want %d", len(report.Results[0].Workloads[0].Metrics.NodeFinalState), tt.want)
			}
		})
	}
}

func TestRunCLIRejectsSeedZero(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "seed-report")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runCLI([]string{
		"--out", outDir,
		"--seed", "0",
		"--profiles", simulator.ProfileDefault,
		"--workloads", simulator.WorkloadBurstShortLived,
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runCLI() error = nil, want invalid --seed")
	}
	const want = "invalid --seed: 0 is reserved; pass a non-zero seed"
	if err.Error() != want {
		t.Fatalf("runCLI() error = %q, want %q", err, want)
	}
	if fileExists(t, outDir) {
		t.Fatal("report written despite invalid --seed")
	}
}

func TestRunCLIRejectsEmptyAndDuplicateCSV(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "empty profiles",
			args: []string{"--profiles", ""},
			want: "invalid --profiles: empty profile name",
		},
		{
			name: "empty profile item",
			args: []string{"--profiles", "default,,balanced_spread"},
			want: "invalid --profiles: empty profile name",
		},
		{
			name: "duplicate profile",
			args: []string{"--profiles", "default,default"},
			want: "duplicate profile",
		},
		{
			name: "empty workloads",
			args: []string{"--workloads", ""},
			want: "invalid --workloads: empty workload name",
		},
		{
			name: "empty workload item",
			args: []string{"--workloads", "burst_short_lived,,mixed_size"},
			want: "invalid --workloads: empty workload name",
		},
		{
			name: "duplicate workload",
			args: []string{"--workloads", "mixed_size,mixed_size"},
			want: "duplicate workload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			outDir := filepath.Join(t.TempDir(), "csv-report")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := runCLI(append([]string{"--out", outDir}, tt.args...), &stdout, &stderr)
			if err == nil {
				t.Fatal("runCLI() error = nil, want CSV validation failure")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runCLI() error = %q, want %q", err, tt.want)
			}
			if fileExists(t, outDir) {
				t.Fatal("report written despite invalid CSV")
			}
		})
	}
}

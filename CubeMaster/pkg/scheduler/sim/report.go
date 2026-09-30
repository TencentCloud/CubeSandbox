// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
)

// BuildVersion returns the VCS revision stamped into the running binary by
// the Go tool ("<revision>", or "<revision>-dirty" when the build tree had
// uncommitted changes), or "" when the binary carries no VCS info (e.g.
// built with -buildvcs=false or outside a VCS checkout).
func BuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// ReportConfig echoes the run parameters into the report so results are
// self-describing. Field set is part of the cross-tool report contract;
// consumers must tolerate the omitempty fields being absent in reports
// written by older binaries.
type ReportConfig struct {
	Tool                  string  `json:"tool"`
	Trace                 string  `json:"trace"`
	Workload              string  `json:"workload"`
	Seed                  int64   `json:"seed"`
	Rounds                int     `json:"rounds"`
	Nodes                 int     `json:"nodes"`
	NodeCPUMillis         int64   `json:"node_cpu_millis"`
	NodeMemMiB            int64   `json:"node_mem_mib"`
	InstanceType          string  `json:"instance_type"`
	TemplatePreload       float64 `json:"template_preload"`
	AllowNonLocalTemplate bool    `json:"allow_non_local_template"`
	TemplateSizeBytes     int64   `json:"template_size_bytes"`
	Requests              int     `json:"requests"`
	// Mode is "quality" (default) or "performance"; empty means quality for
	// reports written before the flag existed.
	Mode string `json:"mode,omitempty"`
	// Version is the code version stamped into the binary by the Go tool
	// ("<vcs revision>" or "<vcs revision>-dirty"); empty when the binary
	// carries no VCS info.
	Version string `json:"version,omitempty"`
	// MetricSyncInterval is the effective scheduler.metric_update_timeout in
	// seconds — the node-metric freshness window the scheduler core checked
	// against during the run.
	MetricSyncInterval float64 `json:"metric_sync_interval,omitempty"`
}

// Report is the schedsim output document: run config, the cross-round mean
// summary, and one entry per round. Summary maps are flat snake_case
// string->number with exactly the keys in SummaryKeys.
type Report struct {
	Config  ReportConfig       `json:"config"`
	Summary map[string]float64 `json:"summary"`
	Rounds  []*RoundResult     `json:"rounds"`
	// Perf is the cross-round performance aggregate, present only in
	// performance mode. Stage percentiles are means of the per-round values.
	Perf *PerfSummary `json:"perf,omitempty"`
}

// MeanSummary averages per-round summaries key by key. Only keys from
// SummaryKeys are emitted, so the aggregated summary has the same shape as a
// round summary.
func MeanSummary(rounds []*RoundResult) map[string]float64 {
	out := make(map[string]float64, len(SummaryKeys))
	if len(rounds) == 0 {
		for _, k := range SummaryKeys {
			out[k] = 0
		}
		return out
	}
	for _, k := range SummaryKeys {
		var sum float64
		for _, r := range rounds {
			sum += r.Summary[k]
		}
		out[k] = sum / float64(len(rounds))
	}
	return out
}

// WriteReport marshals the report indented and writes it to path, or to
// stdout when path is empty.
func WriteReport(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')
	if path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write report %s: %w", path, err)
	}
	return nil
}

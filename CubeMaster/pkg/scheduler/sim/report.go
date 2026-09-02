// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ReportConfig echoes the run parameters into the report so results are
// self-describing. Field set is part of the cross-tool report contract.
type ReportConfig struct {
	Tool            string  `json:"tool"`
	Trace           string  `json:"trace"`
	Workload        string  `json:"workload"`
	Seed            int64   `json:"seed"`
	Rounds          int     `json:"rounds"`
	Nodes           int     `json:"nodes"`
	NodeCPUMillis   int64   `json:"node_cpu_millis"`
	NodeMemMiB      int64   `json:"node_mem_mib"`
	InstanceType    string  `json:"instance_type"`
	TemplatePreload float64 `json:"template_preload"`
	// AllowNonLocalTemplate flips scheduler semantics (strict locality vs
	// remote-place allowed), so it is echoed for cross-run comparability.
	AllowNonLocalTemplate bool `json:"allow_non_local_template"`
	Requests              int  `json:"requests"`
}

// Report is the schedsim output document: run config, the cross-round mean
// summary, and one entry per round. Summary maps are flat snake_case
// string->number with exactly the keys in SummaryKeys.
type Report struct {
	Config  ReportConfig       `json:"config"`
	Summary map[string]float64 `json:"summary"`
	Rounds  []*RoundResult     `json:"rounds"`
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

// TopFailureReasons renders the most frequent failure reasons of a round,
// most frequent first, for the CLI's stderr diagnostics.
func TopFailureReasons(reasons map[string]int, limit int) string {
	type kv struct {
		reason string
		count  int
	}
	rows := make([]kv, 0, len(reasons))
	for reason, count := range reasons {
		rows = append(rows, kv{reason, count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].reason < rows[j].reason
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%dx %s", r.count, r.reason))
	}
	return strings.Join(parts, "; ")
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

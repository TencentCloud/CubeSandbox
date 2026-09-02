// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/sim"
)

// compareOptions carries the single-run flags every compare variant shares;
// only the scheduler config differs between variants (control variables).
type compareOptions struct {
	TracePath    string
	OutDir       string
	Nodes        int
	NodeCPUMilli int64
	NodeMemMiB   int64
	InstanceType string
	Preload      float64
	Seed         int64
	Rounds       int
	Out          string
}

// parseCompareList parses "name=config.yaml,name2=config2.yaml" preserving
// order; the first entry is the baseline.
func parseCompareList(raw string) ([][2]string, error) {
	var out [][2]string
	seen := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, path, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		path = strings.TrimSpace(path)
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("compare: entry %q must be name=config.yaml", entry)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("compare: duplicate variant name %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, [2]string{name, path})
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("compare: need at least 2 variants, got %d", len(out))
	}
	return out, nil
}

// runCompareMode runs one single-config schedsim subprocess per variant and
// renders the markdown comparison report. Subprocesses (not in-process
// re-init) give every variant a pristine config/scheduler/localcache
// singleton, which the CubeMaster config and scheduler packages cannot
// rebuild inside one process.
func runCompareMode(compareList string, opts compareOptions) error {
	variants, err := parseCompareList(compareList)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("compare: locate own executable: %w", err)
	}
	outDir := opts.OutDir
	if outDir == "" {
		dir, err := os.MkdirTemp("", "schedsim-compare-")
		if err != nil {
			return fmt.Errorf("compare: create temp out dir: %w", err)
		}
		outDir = dir
	} else if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("compare: create out dir %s: %w", outDir, err)
	}

	reports := make([]sim.VariantReport, 0, len(variants))
	for _, v := range variants {
		name, cfgPath := v[0], v[1]
		jsonPath := filepath.Join(outDir, name+".json")
		args := []string{
			"--trace", opts.TracePath,
			"--config", cfgPath,
			"--nodes", fmt.Sprint(opts.Nodes),
			"--node-cpu-millis", fmt.Sprint(opts.NodeCPUMilli),
			"--node-mem-mib", fmt.Sprint(opts.NodeMemMiB),
			"--instance-type", opts.InstanceType,
			"--template-preload", fmt.Sprint(opts.Preload),
			"--seed", fmt.Sprint(opts.Seed),
			"--rounds", fmt.Sprint(opts.Rounds),
			"-o", jsonPath,
		}
		fmt.Fprintf(os.Stderr, "schedsim compare: running variant %q (%s)\n", name, cfgPath)
		cmd := exec.Command(exe, args...)
		// Stream the child's progress/errors through; its stdout carries
		// nothing because -o redirects the report to jsonPath.
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("compare: variant %q run: %w", name, err)
		}
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			return fmt.Errorf("compare: variant %q read report: %w", name, err)
		}
		var rep sim.Report
		if err := json.Unmarshal(data, &rep); err != nil {
			return fmt.Errorf("compare: variant %q parse report %s: %w", name, jsonPath, err)
		}
		reports = append(reports, sim.VariantReport{Name: name, ConfigPath: cfgPath, Report: &rep})
	}

	markdown, err := sim.RenderCompare(reports, time.Now().UTC())
	if err != nil {
		return err
	}
	if opts.Out == "" {
		fmt.Print(markdown)
	} else {
		if err := os.WriteFile(opts.Out, []byte(markdown), 0o644); err != nil {
			return fmt.Errorf("compare: write report %s: %w", opts.Out, err)
		}
	}
	fmt.Fprintf(os.Stderr, "schedsim compare: per-variant JSON in %s\n", outDir)
	if opts.Out != "" {
		fmt.Fprintf(os.Stderr, "schedsim compare: report written to %s\n", opts.Out)
	}
	return nil
}

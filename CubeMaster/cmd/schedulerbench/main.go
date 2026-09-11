// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/simulator"
)

const (
	formatJSON     = "json"
	formatMarkdown = "markdown"
	formatBoth     = "both"
	defaultFormat  = formatBoth
	verifyScope    = "default report structural and internal-consistency contract"
	gitTimeout     = 2 * time.Second
)

type provenanceResolver func() simulator.Provenance

type gitCommandRunner func(context.Context, ...string) ([]byte, error)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func runCLI(args []string, stdout, stderr io.Writer) error {
	return runCLIWithProvenance(args, stdout, stderr, buildProvenance)
}

func runCLIWithProvenance(args []string, stdout, stderr io.Writer, resolve provenanceResolver) error {
	defaultConfig := simulator.DefaultConfig()
	flags := flag.NewFlagSet("schedulerbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		outDir    = flags.String("out", "schedulerbench-report", "directory for report.json and report.md")
		seed      = flags.Int64("seed", defaultConfig.Seed, "deterministic workload seed recorded in the report; must be non-zero because 0 is reserved as the library unset sentinel and is rejected here")
		nodeCount = flags.Int("nodes", defaultConfig.NodeCount, "simulated node count (1-4)")
		profiles  = flags.String("profiles", strings.Join(defaultConfig.Profiles, ","), "comma-separated profile list")
		workloads = flags.String("workloads", strings.Join(defaultConfig.Workloads, ","), "comma-separated workload list")
		format    = flags.String("format", defaultFormat, "report format: json, markdown, or both; unselected report.json/report.md files already present in --out are deleted")
		verify    = flags.Bool("verify", false, "check the "+verifyScope+" (not arbitrary --profiles/--workloads subsets; checks structure and internal consistency only, not live scheduling performance or semantic equivalence to production scheduling)")
	)
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return fmt.Errorf("parse scheduler benchmark flags: %w", err)
	}

	if err := validateFormat(*format); err != nil {
		return err
	}
	// Explicit CLI --seed 0 must fail rather than silently becoming the
	// library default via normalizeConfig (0 is the unset sentinel).
	if *seed == 0 {
		return fmt.Errorf("invalid --seed: 0 is reserved; pass a non-zero seed")
	}
	// Explicit CLI --nodes 0 must fail rather than silently becoming the
	// library default via normalizeConfig.
	if err := simulator.ValidateNodeCount(*nodeCount); err != nil {
		return fmt.Errorf("invalid --nodes: %w", err)
	}

	profileList, err := splitCSV(*profiles, "profile")
	if err != nil {
		return err
	}
	workloadList, err := splitCSV(*workloads, "workload")
	if err != nil {
		return err
	}

	cfg := simulator.Config{
		Seed:       *seed,
		NodeCount:  *nodeCount,
		Profiles:   profileList,
		Workloads:  workloadList,
		Provenance: resolve(),
	}
	report, err := simulator.Run(cfg)
	if err != nil {
		return fmt.Errorf("scheduler benchmark failed: %w", err)
	}
	if *verify {
		if err := simulator.VerifyDefaultReport(report); err != nil {
			return fmt.Errorf("scheduler benchmark %s verification failed; use the default --profiles and --workloads values: %w", verifyScope, err)
		}
	}

	if err := writeReports(*outDir, *format, report); err != nil {
		return err
	}
	if *verify {
		fmt.Fprintf(stdout, "scheduler benchmark %s passed (structure and internal consistency only; not live scheduling performance and not semantic equivalence to production scheduling)\n", verifyScope)
	}
	fmt.Fprintf(stdout, "scheduler benchmark report written to %s\n", *outDir)
	return nil
}

// buildProvenance prefers complete VCS stamps from embedded build information,
// including those that the Go toolchain may provide for `go run`. When those
// stamps are unavailable, the CLI narrowly falls back to bounded, read-only
// Git commands. Process execution remains outside the simulator library.
func buildProvenance() simulator.Provenance {
	return resolveProvenance(debug.ReadBuildInfo, runGitCommand)
}

func resolveProvenance(
	readBuildInfo func() (*debug.BuildInfo, bool),
	runGit gitCommandRunner,
) simulator.Provenance {
	if info, ok := readBuildInfo(); ok {
		var revision string
		var dirty *bool
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					value := setting.Value == "true"
					dirty = &value
				}
			}
		}
		// Prefer a known embedded revision even when vcs.modified is absent
		// or unparsable. The Git fallback below likewise keeps revision when
		// dirty-state lookup fails; discarding a known stamp here would risk
		// reporting git_revision "unknown" outside a work tree.
		if revision != "" {
			return simulator.Provenance{GitRevision: revision, GitDirty: dirty}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	revisionBytes, err := runGit(ctx, "rev-parse", "HEAD")
	revision := strings.TrimSpace(string(revisionBytes))
	if err != nil || revision == "" {
		return simulator.Provenance{}
	}
	statusBytes, err := runGit(ctx, "status", "--porcelain")
	if err != nil {
		return simulator.Provenance{GitRevision: revision}
	}
	dirty := len(strings.TrimSpace(string(statusBytes))) > 0
	return simulator.Provenance{GitRevision: revision, GitDirty: &dirty}
}

func runGitCommand(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", args...).Output()
}

func validateFormat(format string) error {
	switch format {
	case formatJSON, formatMarkdown, formatBoth:
		return nil
	default:
		return fmt.Errorf("invalid --format %q: must be json, markdown, or both", format)
	}
}

func writeReports(outDir, format string, report simulator.Report) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	jsonPath := filepath.Join(outDir, "report.json")
	mdPath := filepath.Join(outDir, "report.md")
	if format == formatJSON || format == formatBoth {
		jsonReport, err := report.JSON()
		if err != nil {
			return fmt.Errorf("marshal json report: %w", err)
		}
		if err := os.WriteFile(jsonPath, jsonReport, 0o644); err != nil {
			return fmt.Errorf("write json report: %w", err)
		}
	} else if err := removeReportFile(jsonPath); err != nil {
		return fmt.Errorf("remove stale json report: %w", err)
	}
	if format == formatMarkdown || format == formatBoth {
		if err := os.WriteFile(mdPath, []byte(report.Markdown()), 0o644); err != nil {
			return fmt.Errorf("write markdown report: %w", err)
		}
	} else if err := removeReportFile(mdPath); err != nil {
		return fmt.Errorf("remove stale markdown report: %w", err)
	}
	return nil
}

func removeReportFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func splitCSV(in, kind string) ([]string, error) {
	parts := strings.Split(in, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("invalid --%ss: empty %s name", kind, kind)
		}
		if _, ok := seen[part]; ok {
			return nil, fmt.Errorf("invalid --%ss: duplicate %s %q", kind, kind, part)
		}
		seen[part] = struct{}{}
		result = append(result, part)
	}
	return result, nil
}

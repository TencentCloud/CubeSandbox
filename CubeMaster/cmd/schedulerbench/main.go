// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/simulator"
)

const (
	formatJSON     = "json"
	formatMarkdown = "markdown"
	formatBoth     = "both"
	defaultFormat  = formatBoth
	verifyScope    = "default workload/profile acceptance contract"
)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func runCLI(args []string, stdout, stderr io.Writer) error {
	defaultConfig := simulator.DefaultConfig()
	flags := flag.NewFlagSet("schedulerbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		outDir    = flags.String("out", "schedulerbench-report", "directory for report.json and report.md")
		seed      = flags.Int64("seed", defaultConfig.Seed, "deterministic workload seed recorded in the report")
		nodeCount = flags.Int("nodes", defaultConfig.NodeCount, "simulated node count")
		profiles  = flags.String("profiles", strings.Join(defaultConfig.Profiles, ","), "comma-separated profile list")
		workloads = flags.String("workloads", strings.Join(defaultConfig.Workloads, ","), "comma-separated workload list")
		format    = flags.String("format", defaultFormat, "report format: json, markdown, or both")
		verify    = flags.Bool("verify", false, "verify the default workload/profile acceptance contract (not arbitrary --profiles/--workloads subsets)")
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

	cfg := simulator.Config{
		Seed:      *seed,
		NodeCount: *nodeCount,
		Profiles:  splitCSV(*profiles),
		Workloads: splitCSV(*workloads),
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
		fmt.Fprintf(stdout, "scheduler benchmark %s verification passed\n", verifyScope)
	}
	fmt.Fprintf(stdout, "scheduler benchmark report written to %s\n", *outDir)
	return nil
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
	if format == formatJSON || format == formatBoth {
		jsonReport, err := report.JSON()
		if err != nil {
			return fmt.Errorf("marshal json report: %w", err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "report.json"), jsonReport, 0o644); err != nil {
			return fmt.Errorf("write json report: %w", err)
		}
	}
	if format == formatMarkdown || format == formatBoth {
		if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(report.Markdown()), 0o644); err != nil {
			return fmt.Errorf("write markdown report: %w", err)
		}
	}
	return nil
}

func splitCSV(in string) []string {
	parts := strings.Split(in, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

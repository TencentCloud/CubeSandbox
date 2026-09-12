// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// schedsim replays a request trace (cube-bench --dump-trace format) against
// the real CubeMaster scheduling core in-process, over a simulated fleet of
// homogeneous nodes, and reports scheduling-quality metrics (packing,
// fragmentation, balance, herding, template locality, scheduling latency).
//
// Usage:
//
//	schedsim --trace trace.json --config example.sim.yaml \
//	  --nodes 300 --node-cpu-millis 64000 --node-mem-mib 131072 \
//	  --template-preload 0.3 --seed 42 --rounds 3 -o report.json
//
// Compare mode runs the same trace under several scheduler configs (one
// subprocess per variant, so each gets a clean process-wide scheduler state)
// and renders a markdown A/B report whose first variant is the baseline:
//
//	schedsim --compare legacy=example.sim.yaml,spread=example.profiles.sim.yaml \
//	  --trace trace.json --nodes 300 --rounds 3 -o compare.md --out-dir ./sim-out
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/sim"
)

func main() {
	var (
		tracePath             = flag.String("trace", "", "trace file (cube-bench --dump-trace JSON), required")
		configPath            = flag.String("config", "", "CubeMaster YAML config for the scheduler under test, required")
		compareList           = flag.String("compare", "", "compare mode: comma-separated name=config.yaml variants; the first is the baseline. Supersedes --config")
		outDir                = flag.String("out-dir", "", "compare mode: directory for per-variant JSON reports (default: a fresh temp dir)")
		nodes                 = flag.Int("nodes", 300, "number of simulated homogeneous nodes")
		nodeCPUMilli          = flag.Int64("node-cpu-millis", 64000, "per-node cpu quota in millicores")
		nodeMemMiB            = flag.Int64("node-mem-mib", 131072, "per-node memory quota in MiB")
		instanceType          = flag.String("instance-type", "sim", "instance type all sim nodes register under")
		preload               = flag.Float64("template-preload", 0.3, "fraction of nodes preloaded with a local replica of each template")
		allowNonLocalTemplate = flag.Bool("allow-non-local-template", false, "allow template requests to use nodes without a local replica")
		templateSizeBytes     = flag.Int64("template-size-bytes", 1<<30, "simulated size in bytes of each preloaded template; template_id scoring is boolean (cached=100/0) so size only feeds image_id factor accounting")
		seed                  = flag.Int64("seed", 42, "base seed; round i uses seed+i")
		rounds                = flag.Int("rounds", 1, "number of simulation rounds")
		mode                  = flag.String("mode", "quality", "run mode: quality (virtual clock, scheduling-quality metrics) | performance (staged wall-clock timing of every scheduling decision + throughput)")
		out                   = flag.String("o", "", "report output path (default: stdout)")
	)
	flag.Parse()

	if *tracePath == "" || (*configPath == "" && *compareList == "") {
		fmt.Fprintln(os.Stderr, "schedsim: --trace and (--config or --compare) are required")
		flag.Usage()
		os.Exit(2)
	}
	if *rounds <= 0 {
		fmt.Fprintln(os.Stderr, "schedsim: --rounds must be > 0")
		os.Exit(2)
	}
	if *mode != "quality" && *mode != "performance" {
		fmt.Fprintf(os.Stderr, "schedsim: --mode must be quality or performance, got %q\n", *mode)
		os.Exit(2)
	}

	if *templateSizeBytes <= 0 {
		fmt.Fprintln(os.Stderr, "schedsim: --template-size-bytes must be > 0")
		os.Exit(2)
	}

	if *compareList != "" {
		opts := compareOptions{
			TracePath:             *tracePath,
			OutDir:                *outDir,
			Nodes:                 *nodes,
			NodeCPUMilli:          *nodeCPUMilli,
			NodeMemMiB:            *nodeMemMiB,
			InstanceType:          *instanceType,
			Preload:               *preload,
			Seed:                  *seed,
			Rounds:                *rounds,
			Mode:                  *mode,
			Out:                   *out,
			AllowNonLocalTemplate: *allowNonLocalTemplate,
			TemplateSizeBytes:     *templateSizeBytes,
		}
		if err := runCompareMode(*compareList, opts); err != nil {
			fmt.Fprintf(os.Stderr, "schedsim: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// runSingle(*tracePath, *configPath, *nodes, *nodeCPUMilli, *nodeMemMiB, *instanceType, *preload, *seed, *rounds, *out)
	runSingle(
		*tracePath,
		*configPath,
		*nodes,
		*nodeCPUMilli,
		*nodeMemMiB,
		*instanceType,
		*preload,
		*allowNonLocalTemplate,
		*templateSizeBytes,
		*seed,
		*rounds,
		*mode,
		*out,
	)
}

// runSingle executes one simulation campaign against a single config and
// writes the JSON report.
func runSingle(
	tracePath, configPath string,
	nodes int,
	nodeCPUMilli, nodeMemMiB int64,
	instanceType string,
	preload float64,
	allowNonLocalTemplate bool,
	templateSizeBytes int64,
	seed int64,
	rounds int,
	mode string,
	out string,
) {
	ctx := context.Background()

	// config.Init dumps the whole parsed config to stdout; keep stdout clean
	// for the JSON report when -o is not given.
	restoreStdout := silenceStdout()
	err := sim.Bootstrap(ctx, configPath)
	restoreStdout()
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: bootstrap: %v\n", err)
		os.Exit(1)
	}

	// The effective metric_update_timeout (defaults applied by config.Init) is
	// recorded into the report config as the metric sync interval.
	var metricSyncInterval float64
	if sc := config.GetConfig().Scheduler; sc != nil {
		metricSyncInterval = sc.MetricUpdateTimeout.Seconds()
		if sc.MetricUpdateTimeout < time.Hour {
			fmt.Fprintf(os.Stderr, "schedsim: note: scheduler.metric_update_timeout=%v is short; the sim refreshes "+
				"node metrics on every placement, but a large value (e.g. 86400s) is recommended for slow machines\n",
				sc.MetricUpdateTimeout)
		}
	}

	trace, err := sim.LoadTrace(tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: %v\n", err)
		os.Exit(1)
	}

	results := make([]*sim.RoundResult, 0, rounds)
	for i := 0; i < rounds; i++ {
		roundSeed := seed + int64(i)
		rr, err := sim.RunRound(ctx, sim.Params{
			AllowNonLocalTemplate: allowNonLocalTemplate,
			TemplateSizeBytes:     templateSizeBytes,
			Trace:                 trace,
			Nodes:                 nodes,
			NodeCPUMillis:         nodeCPUMilli,
			NodeMemMiB:            nodeMemMiB,
			InstanceType:          instanceType,
			TemplatePreload:       preload,
			Seed:                  roundSeed,
			RoundID:               i,
			Perf:                  mode == "performance",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "schedsim: round %d: %v\n", i, err)
			os.Exit(1)
		}
		results = append(results, rr)
		fmt.Fprintf(os.Stderr, "schedsim: round %d (seed %d): success_rate=%.4f cpu_alloc_rate=%.4f template_hit_rate=%.4f\n",
			i, roundSeed, rr.Summary["success_rate"], rr.Summary["cpu_alloc_rate"], rr.Summary["template_hit_rate"])
		if rr.Perf != nil {
			fmt.Fprintf(os.Stderr, "schedsim: round %d perf: total_p50=%.3fms total_p99=%.3fms throughput=%.0f decisions/s\n",
				i, rr.Perf.Stages["total"].P50Ms, rr.Perf.Stages["total"].P99Ms, rr.Perf.ThroughputRPS)
			if oh := rr.Perf.Overhead; oh != nil {
				fmt.Fprintf(os.Stderr, "schedsim: round %d overhead: cpu=%.2fs (%.2f cores avg) peak_rss=%.1fMiB heap_alloc=%.1fMiB heap_sys=%.1fMiB\n",
					i, oh.CPUSeconds, oh.AvgCPUCores, oh.PeakRSSMiB, oh.HeapAllocMiB, oh.HeapSysMiB)
			}
		}
	}

	rep := &sim.Report{
		Config: sim.ReportConfig{
			Tool:                  "schedsim",
			Trace:                 tracePath,
			Workload:              trace.Workload,
			Seed:                  seed,
			Rounds:                rounds,
			Nodes:                 nodes,
			NodeCPUMillis:         nodeCPUMilli,
			NodeMemMiB:            nodeMemMiB,
			InstanceType:          instanceType,
			TemplatePreload:       preload,
			AllowNonLocalTemplate: allowNonLocalTemplate,
			TemplateSizeBytes:     templateSizeBytes,
			Requests:              len(trace.Requests),
			Mode:                  mode,
			Version:               sim.BuildVersion(),
			MetricSyncInterval:    metricSyncInterval,
		},
		Summary: sim.MeanSummary(results),
		Rounds:  results,
	}
	if mode == "performance" {
		rep.Perf = sim.AggregatePerf(results, len(trace.Requests))
	}
	if err := sim.WriteReport(out, rep); err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: %v\n", err)
		os.Exit(1)
	}
	if out != "" {
		fmt.Fprintf(os.Stderr, "schedsim: report written to %s\n", out)
	}
}

// silenceStdout swaps os.Stdout for /dev/null until the returned function is
// called. It exists solely to swallow config.Init's one-shot config dump.
func silenceStdout() func() {
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return func() {}
	}
	os.Stdout = devnull
	return func() {
		os.Stdout = old
		_ = devnull.Close()
	}
}

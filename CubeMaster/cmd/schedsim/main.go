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
		tracePath    = flag.String("trace", "", "trace file (cube-bench --dump-trace JSON), required")
		configPath   = flag.String("config", "", "CubeMaster YAML config for the scheduler under test, required")
		nodes        = flag.Int("nodes", 300, "number of simulated homogeneous nodes")
		nodeCPUMilli = flag.Int64("node-cpu-millis", 64000, "per-node cpu quota in millicores")
		nodeMemMiB   = flag.Int64("node-mem-mib", 131072, "per-node memory quota in MiB")
		instanceType = flag.String("instance-type", "sim", "instance type all sim nodes register under")
		preload      = flag.Float64("template-preload", 1.0, "fraction of nodes preloaded with a local replica of each template")
		seed         = flag.Int64("seed", 42, "base seed; round i uses seed+i")
		rounds       = flag.Int("rounds", 1, "number of simulation rounds")
		out          = flag.String("o", "", "report output path (default: stdout)")
	)
	flag.Parse()

	if *tracePath == "" || *configPath == "" {
		fmt.Fprintln(os.Stderr, "schedsim: --trace and --config are both required")
		flag.Usage()
		os.Exit(2)
	}
	if *rounds <= 0 {
		fmt.Fprintln(os.Stderr, "schedsim: --rounds must be > 0")
		os.Exit(2)
	}

	ctx := context.Background()

	// config.Init dumps the whole parsed config to stdout; keep stdout clean
	// for the JSON report when -o is not given. The restore is defer-guarded
	// so a Bootstrap panic cannot strand the swapped os.Stdout.
	err := func() error {
		restoreStdout := silenceStdout()
		defer restoreStdout()
		return sim.Bootstrap(ctx, *configPath)
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: bootstrap: %v\n", err)
		os.Exit(1)
	}

	if sc := config.GetConfig().Scheduler; sc != nil && sc.MetricUpdateTimeout < time.Hour {
		fmt.Fprintf(os.Stderr, "schedsim: note: scheduler.metric_update_timeout=%v is short; the sim refreshes "+
			"node metrics on every placement, but a large value (e.g. 86400s) is recommended for slow machines\n",
			sc.MetricUpdateTimeout)
	}

	trace, err := sim.LoadTrace(*tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: %v\n", err)
		os.Exit(1)
	}

	results := make([]*sim.RoundResult, 0, *rounds)
	for i := 0; i < *rounds; i++ {
		roundSeed := *seed + int64(i)
		rr, err := sim.RunRound(ctx, sim.Params{
			Trace:           trace,
			Nodes:           *nodes,
			NodeCPUMillis:   *nodeCPUMilli,
			NodeMemMiB:      *nodeMemMiB,
			InstanceType:    *instanceType,
			TemplatePreload: *preload,
			Seed:            roundSeed,
			RoundID:         i,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "schedsim: round %d: %v\n", i, err)
			os.Exit(1)
		}
		results = append(results, rr)
		fmt.Fprintf(os.Stderr, "schedsim: round %d (seed %d): success_rate=%.4f cpu_alloc_rate=%.4f template_hit_rate=%.4f\n",
			i, roundSeed, rr.Summary["success_rate"], rr.Summary["cpu_alloc_rate"], rr.Summary["template_hit_rate"])
	}

	rep := &sim.Report{
		Config: sim.ReportConfig{
			Tool:            "schedsim",
			Trace:           *tracePath,
			Workload:        trace.Workload,
			Seed:            *seed,
			Rounds:          *rounds,
			Nodes:           *nodes,
			NodeCPUMillis:   *nodeCPUMilli,
			NodeMemMiB:      *nodeMemMiB,
			InstanceType:    *instanceType,
			TemplatePreload: *preload,
			Requests:        len(trace.Requests),
		},
		Summary: sim.MeanSummary(results),
		Rounds:  results,
	}
	if err := sim.WriteReport(*out, rep); err != nil {
		fmt.Fprintf(os.Stderr, "schedsim: %v\n", err)
		os.Exit(1)
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "schedsim: report written to %s\n", *out)
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

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/*
Sandbox stress test — concurrent create + kill benchmark.

Usage:

	go run . [flags]

Flags:

	-c, --concurrency   Number of parallel workers (default: 5)
	-n, --total         Total iterations across all workers (default: 20)
	-t, --template      Template ID (overrides CUBE_TEMPLATE_ID env var)
	--nodel             Skip sandbox deletion after creation (do not kill, do not record kill latency)

Environment variables (required unless passed as flags):

	CUBE_TEMPLATE_ID  — sandbox template to boot
	E2B_API_KEY       — API key (any non-empty string when auth is disabled)
	E2B_API_URL       — base URL of the Cube API Server, e.g. http://localhost:3000
	SSL_CERT_FILE     — path to CA cert for TLS verification (mkcert HTTPS only)
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"
)

// ─── Result types ─────────────────────────────────────────────────────────

type iterResult struct {
	workerID  int
	iteration int
	createMs  float64
	killMs    float64
	err       string // empty on success
}

// ─── Core worker ──────────────────────────────────────────────────────────

func runWorker(client *Client, workerID, iterations int, templateID string, noDel bool, results chan<- iterResult) {
	for i := range iterations {
		var r iterResult
		r.workerID = workerID
		r.iteration = i

		// ── create ──────────────────────────────────────────────────────────
		t0 := time.Now()
		sb, err := client.Create(context.Background(), templateID)
		r.createMs = float64(time.Since(t0).Microseconds()) / 1000.0

		if err != nil {
			r.err = fmt.Sprintf("create failed: %v", err)
			results <- r
			continue
		}

		// ── kill ─────────────────────────────────────────────────────────────
		if !noDel {
			t0 = time.Now()
			killErr := client.Kill(context.Background(), sb.SandboxID)
			r.killMs = float64(time.Since(t0).Microseconds()) / 1000.0

			if killErr != nil {
				r.err = fmt.Sprintf("kill failed (sandboxID=%s): %v", sb.SandboxID, killErr)
			}
		}
		results <- r
	}
}

// ─── Stats helpers ────────────────────────────────────────────────────────

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := int(float64(len(sorted))*pct/100) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func mean(data []float64) float64 {
	if len(data) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func printStats(label string, values []float64) {
	if len(values) == 0 {
		fmt.Printf("  %s: no data\n", label)
		return
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	fmt.Printf("  %s:\n", label)
	fmt.Printf("    count=%-4d  min=%.1fms  avg=%.1fms  p50=%.1fms  p95=%.1fms  p99=%.1fms  max=%.1fms\n",
		len(sorted),
		sorted[0],
		mean(sorted),
		percentile(sorted, 50),
		percentile(sorted, 95),
		percentile(sorted, 99),
		sorted[len(sorted)-1],
	)
}

// ─── Main ─────────────────────────────────────────────────────────────────

func main() {
	concurrency := flag.Int("c", 5, "Number of parallel workers")
	total := flag.Int("n", 20, "Total iterations across all workers")
	templateFlag := flag.String("t", "", "Template ID (overrides CUBE_TEMPLATE_ID)")
	noDel := flag.Bool("nodel", false, "Skip sandbox deletion after creation (do not kill, do not record kill latency)")
	flag.Parse()

	// ── resolve template ──────────────────────────────────────────────────
	templateID := *templateFlag
	if templateID == "" {
		templateID = os.Getenv("CUBE_TEMPLATE_ID")
	}
	if templateID == "" {
		fmt.Fprintln(os.Stderr, "ERROR: template ID not set. Use -t or set CUBE_TEMPLATE_ID.")
		os.Exit(1)
	}

	if *concurrency < 1 {
		*concurrency = 1
	}
	if *total < 1 {
		*total = 1
	}

	// Distribute iterations as evenly as possible across workers.
	base := *total / *concurrency
	remainder := *total % *concurrency
	itersPerWorker := make([]int, *concurrency)
	for i := range *concurrency {
		itersPerWorker[i] = base
		if i < remainder {
			itersPerWorker[i]++
		}
	}

	// ── init client ───────────────────────────────────────────────────────
	client, err := NewClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n%s\n", divider())
	fmt.Printf("  Sandbox Benchmark\n")
	fmt.Printf("%s\n", divider())
	fmt.Printf("  Template    : %s\n", templateID)
	fmt.Printf("  Concurrency : %d workers\n", *concurrency)
	fmt.Printf("  Total iters : %d  %v per worker\n", *total, itersPerWorker)
	if *noDel {
		fmt.Printf("  Mode        : create-only (--nodel, sandboxes kept alive)\n")
	}
	fmt.Printf("%s\n\n", divider())

	// ── run workers ───────────────────────────────────────────────────────
	resultsCh := make(chan iterResult, *total)
	var wg sync.WaitGroup

	wallStart := time.Now()

	for wid := range *concurrency {
		wg.Add(1)
		go func(id, iters int) {
			defer wg.Done()
			runWorker(client, id, iters, templateID, *noDel, resultsCh)
		}(wid, itersPerWorker[wid])
	}

	// Close channel once all workers finish.
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var allResults []iterResult
	for r := range resultsCh {
		allResults = append(allResults, r)
	}
	wallElapsed := time.Since(wallStart)

	// ── aggregate ─────────────────────────────────────────────────────────
	var createTimes, killTimes []float64
	var errors []string

	for _, r := range allResults {
		if r.err != "" {
			errors = append(errors, fmt.Sprintf("worker=%d iter=%d: %s", r.workerID, r.iteration, r.err))
		} else {
			createTimes = append(createTimes, r.createMs)
			killTimes = append(killTimes, r.killMs)
		}
	}

	fmt.Printf("\n%s\n", divider())
	fmt.Printf("  Results  (successful=%d, errors=%d, wall=%.2fs)\n",
		len(createTimes), len(errors), wallElapsed.Seconds())
	fmt.Printf("%s\n", divider())
	printStats("CREATE", createTimes)
	if !*noDel {
		printStats("KILL  ", killTimes)
	}

	if len(errors) > 0 {
		fmt.Printf("\n  Errors (%d):\n", len(errors))
		for _, e := range errors {
			fmt.Printf("    ✗ %s\n", e)
		}
	}
	fmt.Printf("%s\n\n", divider())

	if len(errors) > 0 {
		os.Exit(1)
	}
}

func divider() string {
	return "────────────────────────────────────────────────────────────"
}

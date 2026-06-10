package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

type IterResult struct {
	Seq      int
	CreateMs float64
	DeleteMs float64
	Err      string
}

func benchOne(client *cubesandbox.Client, cfg *Config, seq int) IterResult {
	r := IterResult{Seq: seq}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// CREATE
	t0 := time.Now()
	sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{TemplateID: cfg.Template})
	r.CreateMs = float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		r.Err = fmt.Sprintf("create: %v", err)
		return r
	}

	// DELETE
	if cfg.Mode == "create-delete" {
		t0 = time.Now()
		err := sandbox.Kill(ctx)
		r.DeleteMs = float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			r.Err = fmt.Sprintf("delete: %v", err)
			return r
		}
	}

	return r
}

func benchOneDry(cfg *Config, seq int) IterResult {
	r := IterResult{Seq: seq}

	createLat := cfg.DryLatencyMean + cfg.DryLatencyStd*rand.NormFloat64()
	if createLat < 1 {
		createLat = 1
	}
	time.Sleep(time.Duration(createLat * float64(time.Millisecond)))
	r.CreateMs = createLat

	if rand.Float64() < cfg.DryErrorRate {
		r.Err = fmt.Sprintf("simulated error (seq=%d)", seq)
		return r
	}

	if cfg.Mode == "create-delete" {
		deleteLat := cfg.DryLatencyMean*0.4 + cfg.DryLatencyStd*0.5*rand.NormFloat64()
		if deleteLat < 1 {
			deleteLat = 1
		}
		time.Sleep(time.Duration(deleteLat * float64(time.Millisecond)))
		r.DeleteMs = deleteLat
	}

	return r
}

func RunBenchmark(cfg *Config, resultCh chan<- IterResult) {
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	var client *cubesandbox.Client
	if !cfg.DryRun {
		// Use a tuned HTTP client for accurate benchmarking.
		httpClient := &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        cfg.Concurrency + 20,
				MaxIdleConnsPerHost: cfg.Concurrency + 20,
				MaxConnsPerHost:     cfg.Concurrency + 20,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 120 * time.Second,
		}

		sdkCfg := cubesandbox.Config{
			APIURL:     cfg.APIURL,
			APIKey:     cfg.APIKey,
			TemplateID: cfg.Template,
		}
		client = cubesandbox.NewClient(sdkCfg, cubesandbox.WithHTTPClient(httpClient))
		defer client.Close()

		// Warmup
		for i := 0; i < cfg.Warmup; i++ {
			r := benchOne(client, cfg, 0)
			if r.Err == "" {
				fmt.Printf("    warmup [%d/%d] ok\n", i+1, cfg.Warmup)
			} else {
				fmt.Printf("    warmup [%d/%d] failed: %s\n", i+1, cfg.Warmup, r.Err)
			}
		}
		if cfg.Warmup > 0 {
			fmt.Println()
		}
	}

	for i := 0; i < cfg.Total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(seq int) {
			defer wg.Done()
			defer func() { <-sem }()
			var r IterResult
			if cfg.DryRun {
				r = benchOneDry(cfg, seq)
			} else {
				r = benchOne(client, cfg, seq)
			}
			resultCh <- r
		}(i + 1)
	}

	wg.Wait()
	close(resultCh)
}

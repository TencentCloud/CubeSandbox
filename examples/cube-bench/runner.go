package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type IterResult struct {
	Seq      int
	CreateMs float64
	DeleteMs float64
	Err      string
}

type createResp struct {
	SandboxID string `json:"sandboxID"`
}

func benchOne(client *http.Client, cfg *Config, seq int) IterResult {
	r := IterResult{Seq: seq}
	apiURL := cfg.APIURL
	headers := map[string]string{"Authorization": "Bearer " + cfg.APIKey}

	body, _ := json.Marshal(map[string]string{"templateID": cfg.Template})

	// CREATE
	t0 := time.Now()
	req, err := http.NewRequest("POST", apiURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		r.Err = fmt.Sprintf("create request build: %v", err)
		return r
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	r.CreateMs = float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		r.Err = fmt.Sprintf("create: %v", err)
		return r
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		msg := string(respBody)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		r.Err = fmt.Sprintf("create HTTP %d: %s", resp.StatusCode, msg)
		return r
	}

	var cr createResp
	if err := json.Unmarshal(respBody, &cr); err != nil {
		r.Err = fmt.Sprintf("create json decode: %v", err)
		return r
	}

	// DELETE
	if cfg.Mode == "create-delete" && cr.SandboxID != "" {
		t0 = time.Now()
		dreq, err := http.NewRequest("DELETE", apiURL+"/sandboxes/"+cr.SandboxID, nil)
		if err != nil {
			r.Err = fmt.Sprintf("delete request build: %v", err)
			return r
		}
		for k, v := range headers {
			dreq.Header.Set(k, v)
		}
		dresp, err := client.Do(dreq)
		r.DeleteMs = float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			r.Err = fmt.Sprintf("delete: %v", err)
			return r
		}
		dresp.Body.Close()
		if dresp.StatusCode != 200 && dresp.StatusCode != 204 {
			r.Err = fmt.Sprintf("delete HTTP %d", dresp.StatusCode)
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

	var client *http.Client
	if !cfg.DryRun {
		client = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        cfg.Concurrency + 20,
				MaxIdleConnsPerHost: cfg.Concurrency + 20,
				MaxConnsPerHost:     cfg.Concurrency + 20,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 120 * time.Second,
		}

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

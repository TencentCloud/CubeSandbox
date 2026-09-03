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

	// Scheduled-workload diagnostics. All zero in legacy mode.
	TemplateID         string
	ScheduledArrivalMs float64 // planned arrival offset from bench start
	ActualStartMs      float64 // actual goroutine start offset from bench start
	SchedDelayMs       float64 // ActualStartMs - ScheduledArrivalMs (queueing delay)
	LifetimeMs         float64
}

type createResp struct {
	SandboxID string `json:"sandboxID"`
}

// dryRunMaxLifetimeSleep caps the per-sandbox occupancy sleep in dry-run
// scheduled mode. Dry-run exists for fast determinism smoke tests, not
// occupancy modeling, so lifetime-bearing presets must not stall for hours;
// the planned lifetime is still recorded in IterResult.LifetimeMs.
const dryRunMaxLifetimeSleep = 25 * time.Millisecond

func benchOne(client *http.Client, cfg *Config, seq int) IterResult {
	return doBenchCycle(client, cfg, cfg.requestBody, seq, 0)
}

// doBenchCycle runs one create(+delete) cycle with an explicit request body.
// deleteDelay > 0 makes the client wait that long after a successful create
// before issuing the DELETE (scheduled lifetime); 0 deletes immediately.
func doBenchCycle(client *http.Client, cfg *Config, body []byte, seq int, deleteDelay time.Duration) IterResult {
	r := IterResult{Seq: seq}
	apiURL := cfg.APIURL

	// CREATE
	t0 := time.Now()
	req, err := http.NewRequest("POST", apiURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		r.Err = fmt.Sprintf("create request build: %v", err)
		return r
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.requestHeaders {
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
		if deleteDelay > 0 {
			time.Sleep(deleteDelay)
		}
		t0 = time.Now()
		dreq, err := http.NewRequest("DELETE", apiURL+"/sandboxes/"+cr.SandboxID, nil)
		if err != nil {
			r.Err = fmt.Sprintf("delete request build: %v", err)
			return r
		}
		for k, v := range cfg.requestHeaders {
			dreq.Header.Set(k, v)
		}
		dresp, err := client.Do(dreq)
		r.DeleteMs = float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			r.Err = fmt.Sprintf("delete: %v", err)
			return r
		}
		defer dresp.Body.Close()
		if dresp.StatusCode != 200 && dresp.StatusCode != 204 {
			r.Err = fmt.Sprintf("delete HTTP %d", dresp.StatusCode)
			return r
		}
	}

	return r
}

// benchOneScheduled runs one pre-generated request: per-request template,
// server-side TTL hint (timeout = trunc(lifetime) + 60s), and a client-side
// DELETE once the lifetime elapses.
//
// Note: the create `timeout` is an *idle* timeout (see
// docs/guide/lifecycle.md) — any sandbox activity refreshes the deadline.
// cube-bench never touches a sandbox after create, so here it behaves like a
// wall-clock fallback cap; the client-side DELETE remains the primary
// lifetime enforcement.
func benchOneScheduled(client *http.Client, cfg *Config, sr ScheduledRequest) IterResult {
	var timeoutS *int64
	if sr.Lifetime > 0 {
		t := int64(sr.Lifetime.Seconds()) + 60
		timeoutS = &t
	}
	body, err := buildCreateRequestBodyWithTimeout(sr.TemplateID, cfg.hostMountValue, cfg.NetworkPolicy, timeoutS)
	if err != nil {
		return IterResult{Seq: sr.Seq, Err: fmt.Sprintf("create request body build: %v", err)}
	}
	return doBenchCycle(client, cfg, body, sr.Seq, sr.Lifetime)
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

// benchOneDryScheduled simulates one scheduled request. Randomness comes from
// a per-request rng derived from (seed, seq), so a fixed --seed reproduces the
// exact same latencies/errors regardless of goroutine scheduling. The
// occupancy sleep is clamped to dryRunMaxLifetimeSleep: dry-run is the fast
// smoke path, and with real lifetimes a lifetime-bearing preset (e.g. burst =
// 500 × 10–120s) cannot finish in reasonable time at moderate concurrency.
// The recorded LifetimeMs still carries the planned value.
func benchOneDryScheduled(cfg *Config, sr ScheduledRequest) IterResult {
	rng := rand.New(rand.NewSource(cfg.Seed*1000003 + int64(sr.Seq) + 1))
	r := IterResult{Seq: sr.Seq}

	createLat := cfg.DryLatencyMean + cfg.DryLatencyStd*rng.NormFloat64()
	if createLat < 1 {
		createLat = 1
	}
	time.Sleep(time.Duration(createLat * float64(time.Millisecond)))
	r.CreateMs = createLat

	if rng.Float64() < cfg.DryErrorRate {
		r.Err = fmt.Sprintf("simulated error (seq=%d)", sr.Seq)
		return r
	}

	if cfg.Mode == "create-delete" {
		if lt := min(sr.Lifetime, dryRunMaxLifetimeSleep); lt > 0 {
			time.Sleep(lt)
		}
		deleteLat := cfg.DryLatencyMean*0.4 + cfg.DryLatencyStd*0.5*rng.NormFloat64()
		if deleteLat < 1 {
			deleteLat = 1
		}
		time.Sleep(time.Duration(deleteLat * float64(time.Millisecond)))
		r.DeleteMs = deleteLat
	}

	return r
}

func newHTTPClient(concurrency int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        concurrency + 20,
			MaxIdleConnsPerHost: concurrency + 20,
			MaxConnsPerHost:     concurrency + 20,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 120 * time.Second,
	}
}

// RunWarmup completes before the benchmark UI starts so its output cannot
// interfere with Bubble Tea's terminal rendering. Warmup time is intentionally
// excluded from the measured benchmark duration.
func RunWarmup(cfg *Config, out io.Writer) *http.Client {
	if cfg.DryRun || cfg.Warmup == 0 {
		return nil
	}

	client := newHTTPClient(cfg.Concurrency)
	for i := 0; i < cfg.Warmup; i++ {
		r := benchOne(client, cfg, 0)
		if r.Err == "" {
			fmt.Fprintf(out, "    warmup [%d/%d] ok\n", i+1, cfg.Warmup)
		} else {
			fmt.Fprintf(out, "    warmup [%d/%d] failed: %s\n", i+1, cfg.Warmup, r.Err)
		}
	}
	fmt.Fprintln(out)
	return client
}

func RunBenchmark(cfg *Config, resultCh chan<- IterResult, client *http.Client) {
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	if !cfg.DryRun && client == nil {
		client = newHTTPClient(cfg.Concurrency)
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

// RunScheduled dispatches a pre-generated request sequence: the dispatcher
// sleeps until each request's scheduled arrival offset, then blocks on the
// concurrency semaphore before releasing the goroutine. The gap between
// scheduled arrival and actual goroutine start is reported as SchedDelayMs.
// Closing stop (e.g. an early TUI quit) ends dispatch: requests not yet
// released are skipped. Released ones are NOT waited on at quit — main
// returns and the process may exit mid create/lifetime/delete, so an
// abandoned sandbox relies on the server-side timeout backstop (present
// only in lifetime-bearing runs). A nil stop channel never fires.
func RunScheduled(cfg *Config, sched []ScheduledRequest, resultCh chan<- IterResult, client *http.Client, stop <-chan struct{}) {
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	if !cfg.DryRun && client == nil {
		client = newHTTPClient(cfg.Concurrency)
	}

	benchStart := time.Now()
dispatch:
	for i := range sched {
		sr := sched[i]
		if d := time.Until(benchStart.Add(sr.ArrivalOffset)); d > 0 {
			timer := time.NewTimer(d)
			select {
			case <-timer.C:
			case <-stop:
				timer.Stop()
				break dispatch
			}
		}
		// The arrival-timer branch above is the only stop check while the
		// dispatcher is on pace; once it falls behind (arrival offsets already
		// in the past, e.g. saturation or ASAP mode) that branch is skipped,
		// and in the two-case select below a ready semaphore slot would
		// compete with the closed stop channel at random, releasing requests
		// after quit. Re-check stop first so a quit under back-pressure stops
		// dispatch deterministically.
		select {
		case <-stop:
			break dispatch
		default:
		}
		wg.Add(1)
		select {
		case sem <- struct{}{}:
		case <-stop:
			// Stopped while back-pressured on the semaphore: undo the Add and
			// end dispatch instead of releasing another sandbox after quit.
			wg.Done()
			break dispatch
		}
		// Count the release only after the semaphore admits the request, so
		// the TUI in-flight gauge tracks live sandboxes instead of the
		// planned schedule (which degenerates to "remaining" in ASAP mode
		// and over-counts under back-pressure).
		cfg.released.Add(1)
		go func(sr ScheduledRequest, actualStart time.Duration) {
			defer wg.Done()
			defer func() { <-sem }()
			var r IterResult
			if cfg.DryRun {
				r = benchOneDryScheduled(cfg, sr)
			} else {
				r = benchOneScheduled(client, cfg, sr)
			}
			r.TemplateID = sr.TemplateID
			r.ScheduledArrivalMs = float64(sr.ArrivalOffset.Microseconds()) / 1000.0
			r.ActualStartMs = float64(actualStart.Microseconds()) / 1000.0
			r.SchedDelayMs = r.ActualStartMs - r.ScheduledArrivalMs
			r.LifetimeMs = float64(sr.Lifetime.Microseconds()) / 1000.0
			resultCh <- r
		}(sr, time.Since(benchStart))
	}

	// The dispatch window closes once the last request has been released;
	// wg.Wait() below additionally waits out per-sandbox lifetime tails,
	// which would otherwise dilute the arrival-side dispatch rate.
	cfg.setDispatchElapsed(time.Since(benchStart).Seconds())

	wg.Wait()
	close(resultCh)
}

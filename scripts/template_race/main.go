// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// template_race is a race-focused stress tool for the template register /
// delete protocol. It complements scripts/template_stress (which staggers
// full lifecycles): this tool fires operations at the SAME instant to hit
// the concurrency windows the register lock / delete CAS / conditional
// callback update were built for.
//
// Scenarios:
//
//	burst  -c concurrent create-from-image of the SAME image spec (same
//	       template_spec_fingerprint, distinct template_ids) released at one
//	       instant. Asserts: every template reaches READY; zero failures
//	       matching /Duplicate entry|idx_artifact_fingerprint/.
//	redo   one template to READY, then -r concurrent redo requests.
//	       Asserts: template is READY afterwards; no duplicate-key errors.
//	churn  -rounds of create -> READY -> delete -> confirm-130404 on the
//	       same spec (exercises the reuse/adopt path and the delete
//	       protocol: CLEANUP_PENDING <-> claim, DELETING CAS, backstop).
//
// Usage:
//
//	go run ./scripts/template_race \
//	  -addr http://127.0.0.1:8089 \
//	  -image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
//	  -mode all -c 8 -r 6 -rounds 5
//
// Exit code is non-zero when any assertion fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	addr         string
	image        string
	instanceType string
	mode         string
	burst        int
	redoCount    int
	rounds       int
	pollInterval time.Duration
	buildTimeout time.Duration
	httpTimeout  time.Duration
	keep         bool
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.addr, "addr", "http://127.0.0.1:8089", "CubeMaster base URL")
	flag.StringVar(&c.image, "image", "", "source image ref (required)")
	flag.StringVar(&c.instanceType, "instance-type", "cubebox", "instance type")
	flag.StringVar(&c.mode, "mode", "all", "scenario: all | burst | redo | churn")
	flag.IntVar(&c.burst, "c", 8, "burst: concurrent same-spec creates")
	flag.IntVar(&c.redoCount, "r", 6, "redo: concurrent redo requests")
	flag.IntVar(&c.rounds, "rounds", 5, "churn: create/delete rounds")
	flag.DurationVar(&c.pollInterval, "poll", 2*time.Second, "status poll interval")
	flag.DurationVar(&c.buildTimeout, "timeout", 15*time.Minute, "max wait for one build/redo to terminate")
	flag.DurationVar(&c.httpTimeout, "http-timeout", 60*time.Second, "per-request HTTP timeout")
	flag.BoolVar(&c.keep, "keep", false, "keep templates after the run (skip delete)")
	flag.Parse()
	if c.image == "" {
		fmt.Fprintln(os.Stderr, "-image is required")
		os.Exit(2)
	}
	return c
}

// ---------------------------------------------------------------------------
// Minimal CubeMaster API client (same envelope as scripts/template_stress)
// ---------------------------------------------------------------------------

const retCodeSuccess = 200 // errorcode.ErrorCode_Success

type envelope struct {
	Ret struct {
		Code int    `json:"ret_code"`
		Msg  string `json:"ret_msg"`
	} `json:"ret"`
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

type apiClient struct {
	base string
	hc   *http.Client
}

func (a *apiClient) do(ctx context.Context, method, path string, body any) (*envelope, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	env := &envelope{}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, fmt.Errorf("decode response: %w (http %d, body %.200s)", err, resp.StatusCode, raw)
	}
	if env.Ret.Code != retCodeSuccess {
		return env, fmt.Errorf("ret_code=%d ret_msg=%s", env.Ret.Code, env.Ret.Msg)
	}
	return env, nil
}

func (a *apiClient) createFromImage(ctx context.Context, tplID, requestID string) error {
	_, err := a.do(ctx, http.MethodPost, "/cube/template/from-image", map[string]any{
		"RequestID":        requestID,
		"template_id":      tplID,
		"source_image_ref": cfg.image,
		"instance_type":    cfg.instanceType,
	})
	return err
}

func (a *apiClient) redo(ctx context.Context, tplID, requestID string) error {
	_, err := a.do(ctx, http.MethodPost, "/cube/template/redo", map[string]any{
		"RequestID":   requestID,
		"template_id": tplID,
	})
	return err
}

func (a *apiClient) delete(ctx context.Context, tplID string) error {
	_, err := a.do(ctx, http.MethodDelete, "/cube/template", map[string]any{"template_id": tplID})
	return err
}

// getStatus returns the template status; a 130404 (not found) right after
// create is reported as an empty status so callers keep polling.
func (a *apiClient) getStatus(ctx context.Context, tplID string) (status, lastErr string, err error) {
	env, err := a.do(ctx, http.MethodGet, "/cube/template?template_id="+tplID, nil)
	if err != nil {
		if strings.Contains(err.Error(), "ret_code=130404") {
			return "", "", nil
		}
		return "", "", err
	}
	return env.Status, env.LastError, nil
}

// waitTerminal polls until the template is READY (true) or FAILED /
// timeout (false). The FAILED last_error is returned for assertion.
func (a *apiClient) waitTerminal(ctx context.Context, tplID string) (bool, string) {
	deadline := time.Now().Add(cfg.buildTimeout)
	for {
		status, lastErr, err := a.getStatus(ctx, tplID)
		if err == nil {
			switch strings.ToUpper(status) {
			case "READY":
				return true, ""
			case "FAILED":
				return false, lastErr
			}
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("timeout after %s (last status=%q err=%v)", cfg.buildTimeout, status, err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err().Error()
		case <-time.After(cfg.pollInterval):
		}
	}
}

func (a *apiClient) confirmGone(ctx context.Context, tplID string) error {
	_, err := a.do(ctx, http.MethodGet, "/cube/template?template_id="+tplID, nil)
	if err == nil {
		return fmt.Errorf("template still readable after delete")
	}
	if !strings.Contains(err.Error(), "ret_code=130404") {
		return fmt.Errorf("expected 130404 after delete, got: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// dupKeyRe matches the fingerprint unique-index violation the register lock
// exists to prevent (Error 1062 on idx_artifact_fingerprint).
var dupKeyRe = regexp.MustCompile(`(?i)duplicate entry|idx_artifact_fingerprint`)

type result struct {
	name     string
	total    int
	failed   int
	dupKey   int
	errors   []string
	duration time.Duration
}

var results []result

func (r *result) recordErr(errs []string) {
	for _, e := range errs {
		if e == "" {
			continue
		}
		r.failed++
		if dupKeyRe.MatchString(e) {
			r.dupKey++
		}
		if len(r.errors) < 8 {
			r.errors = append(r.errors, e)
		}
	}
}

func (r *result) report() {
	status := "PASS"
	if r.failed > 0 {
		status = "FAIL"
	}
	fmt.Printf("\n[%s] %s: total=%d failed=%d dup_key_violations=%d (took %s)\n",
		status, r.name, r.total, r.failed, r.dupKey, r.duration.Round(time.Millisecond))
	for _, e := range r.errors {
		fmt.Printf("    err: %s\n", e)
	}
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

var cfg config
var api *apiClient

// fireAt releases all fn() calls at the same instant.
func fireAt(ctx context.Context, n int, fn func(i int) string) []string {
	start := make(chan struct{})
	errs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// scenarioBurst: -c concurrent same-spec creates released at one instant.
// Every build shares one fingerprint, so concurrent BUILT callbacks race on
// the register lock — before the fix this produced Error 1062 on
// idx_artifact_fingerprint.
func scenarioBurst(ctx context.Context) {
	res := result{name: "burst(same-spec creates)", total: cfg.burst}
	defer func() { results = append(results, res) }()
	start := time.Now()
	defer func() { res.duration = time.Since(start) }()

	tplIDs := make([]string, cfg.burst)
	for i := range tplIDs {
		tplIDs[i] = fmt.Sprintf("tpl-race-burst-%d-%d", time.Now().UnixNano()%1_000_000, i)
	}

	fmt.Printf("burst: firing %d concurrent creates of %s\n", cfg.burst, cfg.image)
	submitErrs := fireAt(ctx, cfg.burst, func(i int) string {
		if err := api.createFromImage(ctx, tplIDs[i], fmt.Sprintf("race-burst-%d", i)); err != nil {
			return fmt.Sprintf("submit %s: %v", tplIDs[i], err)
		}
		return ""
	})
	res.recordErr(submitErrs)

	var readyCount, failCount atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, tplID := range tplIDs {
		if submitErrs[i] != "" {
			continue
		}
		wg.Add(1)
		go func(tplID string) {
			defer wg.Done()
			ok, lastErr := api.waitTerminal(ctx, tplID)
			if ok {
				readyCount.Add(1)
				return
			}
			failCount.Add(1)
			mu.Lock()
			res.recordErr([]string{fmt.Sprintf("%s: %s", tplID, lastErr)})
			mu.Unlock()
		}(tplID)
	}
	wg.Wait()
	fmt.Printf("burst: %d/%d READY, %d failed\n", readyCount.Load(), cfg.burst, failCount.Load())

	if !cfg.keep {
		for _, tplID := range tplIDs {
			_ = api.delete(ctx, tplID)
		}
	}
}

// scenarioRedo: one template to READY, then -r concurrent redo requests.
// Redo resumes at DISTRIBUTING and re-reads the artifact; concurrent redos
// exercise the artifact load/lock discipline without a rebuild.
func scenarioRedo(ctx context.Context) {
	res := result{name: "redo(concurrent)", total: cfg.redoCount}
	defer func() { results = append(results, res) }()
	start := time.Now()
	defer func() { res.duration = time.Since(start) }()

	tplID := fmt.Sprintf("tpl-race-redo-%d", time.Now().UnixNano()%1_000_000)
	fmt.Printf("redo: building %s first\n", tplID)
	if err := api.createFromImage(ctx, tplID, "race-redo-seed"); err != nil {
		res.recordErr([]string{fmt.Sprintf("seed create: %v", err)})
		return
	}
	if ok, lastErr := api.waitTerminal(ctx, tplID); !ok {
		res.recordErr([]string{fmt.Sprintf("seed build: %s", lastErr)})
		return
	}

	fmt.Printf("redo: firing %d concurrent redos of %s\n", cfg.redoCount, tplID)
	redoErrs := fireAt(ctx, cfg.redoCount, func(i int) string {
		if err := api.redo(ctx, tplID, fmt.Sprintf("race-redo-%d", i)); err != nil {
			return fmt.Sprintf("redo submit: %v", err)
		}
		return ""
	})
	res.recordErr(redoErrs)

	// All redos are terminal when the template reads READY again and no redo
	// job is in flight; waitTerminal covers the template reaching READY.
	if ok, lastErr := api.waitTerminal(ctx, tplID); !ok {
		res.recordErr([]string{fmt.Sprintf("post-redo state: %s", lastErr)})
	}

	if !cfg.keep {
		_ = api.delete(ctx, tplID)
	}
}

// scenarioChurn: create -> READY -> delete -> confirm-gone on the same spec,
// -rounds times. Exercises the reuse/adopt path (the artifact row survives
// between rounds only if delete loses to a concurrent claim) and the delete
// protocol (CLEANUP_PENDING drain, DELETING CAS, backstop sweep).
func scenarioChurn(ctx context.Context) {
	res := result{name: "churn(create/delete same spec)", total: cfg.rounds}
	defer func() { results = append(results, res) }()
	start := time.Now()
	defer func() { res.duration = time.Since(start) }()

	for round := 0; round < cfg.rounds; round++ {
		if ctx.Err() != nil {
			return
		}
		tplID := fmt.Sprintf("tpl-race-churn-%d-%d", time.Now().UnixNano()%1_000_000, round)
		if err := api.createFromImage(ctx, tplID, fmt.Sprintf("race-churn-%d", round)); err != nil {
			res.recordErr([]string{fmt.Sprintf("round %d create: %v", round, err)})
			continue
		}
		if ok, lastErr := api.waitTerminal(ctx, tplID); !ok {
			res.recordErr([]string{fmt.Sprintf("round %d build: %s", round, lastErr)})
			_ = api.delete(ctx, tplID)
			continue
		}
		if cfg.keep {
			continue
		}
		if err := api.delete(ctx, tplID); err != nil {
			res.recordErr([]string{fmt.Sprintf("round %d delete: %v", round, err)})
			continue
		}
		if err := api.confirmGone(ctx, tplID); err != nil {
			res.recordErr([]string{fmt.Sprintf("round %d confirm: %v", round, err)})
		}
		fmt.Printf("churn: round %d/%d done\n", round+1, cfg.rounds)
	}
}

// ---------------------------------------------------------------------------

func main() {
	cfg = parseFlags()
	api = &apiClient{
		base: strings.TrimRight(cfg.addr, "/"),
		hc:   &http.Client{Timeout: cfg.httpTimeout},
	}
	ctx := context.Background()

	fmt.Printf("template race: addr=%s image=%s mode=%s c=%d r=%d rounds=%d\n",
		cfg.addr, cfg.image, cfg.mode, cfg.burst, cfg.redoCount, cfg.rounds)
	start := time.Now()

	run := func(name string, fn func(context.Context)) {
		if cfg.mode == "all" || cfg.mode == name {
			fn(ctx)
		}
	}
	run("burst", scenarioBurst)
	run("redo", scenarioRedo)
	run("churn", scenarioChurn)

	fmt.Printf("\n===== summary (elapsed %s) =====", time.Since(start).Round(time.Second))
	failed := false
	for _, r := range results {
		r.report()
		if r.failed > 0 {
			failed = true
		}
	}
	if failed {
		fmt.Println("\nRESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("\nRESULT: PASS")
}

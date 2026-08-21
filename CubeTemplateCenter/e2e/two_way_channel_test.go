// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package e2e exercises the full two-way channel between CubeMaster and
// CubeTemplateCenter in-process, without docker or root:
//
//	forward:  HTTP POST /tc/api/v1/build  -> route -> Executor.Submit -> build
//	reverse:  build -> Reporter.Report    -> HTTP POST <master>/internal/...
//
// The image pull / mkfs work inside Build is replaced by a seam that still
// reports through the real Reporter, because that work needs Linux + root and
// is not what the channel is being tested for. What IS real here: the HTTP
// server, the route, the executor lifecycle, the reporter's HTTP client, and
// the master-side endpoint receiving callbacks.
//
// These run as `go test ./e2e/` on any platform (no unix-specific imports).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/api"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/build"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
)

// statusCall is one callback the mock CubeMaster received.
type statusCall struct {
	JobID string
	Body  map[string]any
}

// mockMaster stands in for CubeMaster's status-callback endpoint, recording
// every POST /internal/template/jobs/:id/status in order.
type mockMaster struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []statusCall
	failNext int
}

func newMockMaster(t *testing.T) *mockMaster {
	t.Helper()
	m := &mockMaster{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/internal/template/jobs/"
		const suffix = "/status"
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		jobID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if m.failNext > 0 {
			m.failNext--
			m.mu.Unlock()
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		m.calls = append(m.calls, statusCall{JobID: jobID, Body: body})
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockMaster) url() string { return m.srv.URL }

func (m *mockMaster) snapshot() []statusCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]statusCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// tcServer is a real gin engine serving TC's internal routes, backed by a real
// Executor whose build is a seam.
type tcServer struct {
	srv *httptest.Server
}

func newTCServer(t *testing.T, e *build.Executor) *tcServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api.SetBuildExecutor(e)
	api.RegisterInternalRoutes(engine.Group(""))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { api.SetBuildExecutor(nil) })
	return &tcServer{srv: srv}
}

// submitBuild performs the forward-channel call exactly as CubeMaster's
// tcclient does.
func (ts *tcServer) submitBuild(t *testing.T, jobID string) *http.Response {
	t.Helper()
	payload := map[string]any{
		"job_id":            jobID,
		"request":           map[string]any{"source_image_ref": "nginx:latest"},
		"download_base_url": "http://master:8089",
	}
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(ts.srv.URL+"/tc/api/v1/build", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("forward channel POST failed: %v", err)
	}
	return resp
}

func waitForStatus(t *testing.T, m *mockMaster, jobID, wantStatus, wantPhase string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range m.snapshot() {
			if c.JobID == jobID && c.Body["status"] == wantStatus && (wantPhase == "" || c.Body["phase"] == wantPhase) {
				return c.Body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("master never received status=%q phase=%q for job %s; got %v", wantStatus, wantPhase, jobID, m.snapshot())
	return nil
}

type phase struct {
	status, ph string
	progress   int
}

// reportingBuild returns a build seam that drives the REAL Reporter against the
// mock master through the given phases, keeping the reverse channel end-to-end
// real while short-circuiting only the image work.
func reportingBuild(phases []phase) func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
	return func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		rep := build.NewReporter() // reads CUBE_MASTER_ADDR, set by the test to the mock master
		defer rep.Close()
		for _, p := range phases {
			if err := rep.Report(ctx, jobID, map[string]any{
				"status":   p.status,
				"phase":    p.ph,
				"progress": p.progress,
			}); err != nil {
				return fmt.Errorf("report %s/%s: %w", p.status, p.ph, err)
			}
		}
		return nil
	}
}

// Full two-way happy path: forward submit accepted, reverse phases arrive in order.
func TestTwoWayChannelSuccess(t *testing.T) {
	master := newMockMaster(t)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.url())

	executor := build.NewExecutor(0)
	executor.SetLookupFunc(func(context.Context, string) error { return nil })
	executor.SetBuildFunc(reportingBuild([]phase{
		{"RUNNING", "pulling_image", 20},
		{"RUNNING", "building_ext4", 60},
		{"BUILT", "ready", 100},
	}))
	defer executor.Shutdown()
	tc := newTCServer(t, executor)

	resp := tc.submitBuild(t, "job-e2e-1")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward channel: got %d, want 200", resp.StatusCode)
	}

	body := waitForStatus(t, master, "job-e2e-1", "BUILT", "ready", 3*time.Second)
	if body["progress"] != float64(100) {
		t.Fatalf("final progress = %v, want 100", body["progress"])
	}

	var sawRunning, sawBuilt bool
	for _, c := range master.snapshot() {
		if c.Body["status"] == "RUNNING" {
			if sawBuilt {
				t.Fatalf("RUNNING reported after BUILT; out of order: %v", master.snapshot())
			}
			sawRunning = true
		}
		if c.Body["status"] == "BUILT" {
			sawBuilt = true
		}
	}
	if !sawRunning || !sawBuilt {
		t.Fatalf("expected both RUNNING and BUILT callbacks, got %v", master.snapshot())
	}
}

// Forward input validation: malformed body -> 400, and no reverse traffic.
func TestForwardChannelRejectsBadBody(t *testing.T) {
	master := newMockMaster(t)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.url())
	executor := build.NewExecutor(0)
	executor.SetLookupFunc(func(context.Context, string) error { return nil })
	defer executor.Shutdown()
	tc := newTCServer(t, executor)

	resp, err := http.Post(tc.srv.URL+"/tc/api/v1/build", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: got %d, want 400", resp.StatusCode)
	}
	if got := len(master.snapshot()); got != 0 {
		t.Fatalf("rejected submit must produce no reverse traffic, got %d calls", got)
	}
}

// Reverse resilience: master fails the first callback; reporter retry still
// delivers the phase without failing the build. Uses the reporter's real
// backoff (base 500ms), so the timeout is generous.
func TestReverseChannelRetriesOnMasterFailure(t *testing.T) {
	master := newMockMaster(t)
	master.failNext = 1
	t.Setenv(tcconfig.EnvMasterEndpoint, master.url())

	executor := build.NewExecutor(0)
	executor.SetLookupFunc(func(context.Context, string) error { return nil })
	executor.SetBuildFunc(reportingBuild([]phase{
		{"RUNNING", "building_ext4", 60},
		{"BUILT", "ready", 100},
	}))
	defer executor.Shutdown()
	tc := newTCServer(t, executor)

	resp := tc.submitBuild(t, "job-e2e-retry")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward channel: got %d", resp.StatusCode)
	}

	waitForStatus(t, master, "job-e2e-retry", "RUNNING", "building_ext4", 5*time.Second)
	waitForStatus(t, master, "job-e2e-retry", "BUILT", "ready", 5*time.Second)
}

// Forward backpressure: at capacity a second submit is 429 while the first runs,
// and only the accepted job produces reverse traffic.
func TestForwardChannelConcurrencyLimit(t *testing.T) {
	master := newMockMaster(t)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.url())

	release := make(chan struct{})
	executor := build.NewExecutor(1)
	executor.SetLookupFunc(func(context.Context, string) error { return nil })
	executor.SetBuildFunc(func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		rep := build.NewReporter()
		defer rep.Close()
		_ = rep.Report(ctx, jobID, map[string]any{"status": "RUNNING", "phase": "building_ext4", "progress": 50})
		<-release
		return nil
	})
	defer executor.Shutdown()
	tc := newTCServer(t, executor)

	resp1 := tc.submitBuild(t, "job-e2e-cap-1")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first submit: got %d, want 200", resp1.StatusCode)
	}
	waitForStatus(t, master, "job-e2e-cap-1", "RUNNING", "building_ext4", 3*time.Second)

	resp2 := tc.submitBuild(t, "job-e2e-cap-2")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second submit at capacity: got %d, want 429", resp2.StatusCode)
	}
	close(release)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, c := range master.snapshot() {
			if c.JobID == "job-e2e-cap-2" {
				t.Fatalf("rejected job produced reverse traffic: %v", c)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Duplicate submit: the second identical job_id is 409 and starts no second build.
func TestForwardChannelDuplicateRejected(t *testing.T) {
	master := newMockMaster(t)
	t.Setenv(tcconfig.EnvMasterEndpoint, master.url())

	release := make(chan struct{})
	executor := build.NewExecutor(0)
	executor.SetLookupFunc(func(context.Context, string) error { return nil })
	executor.SetBuildFunc(func(ctx context.Context, jobID string, _ *types.CreateTemplateFromImageReq, _, _ string, _ []byte) error {
		<-release
		return nil
	})
	defer executor.Shutdown()
	tc := newTCServer(t, executor)

	resp1 := tc.submitBuild(t, "job-e2e-dup")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first submit: got %d", resp1.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for executor.InFlight() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("build never entered in-flight state")
		}
		time.Sleep(time.Millisecond)
	}

	resp2 := tc.submitBuild(t, "job-e2e-dup")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate submit: got %d, want 409", resp2.StatusCode)
	}
	close(release)
}

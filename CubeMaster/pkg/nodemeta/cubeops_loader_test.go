// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestCubeOpsLoader_LoadNodes(t *testing.T) {
	want := []*node.Node{
		{
			InsID:       "node-1",
			IP:          "10.0.0.1",
			CpuTotal:    4,
			MemMBTotal:  8192,
			Healthy:     true,
			NodeLabels:  map[string]string{"zone": "gz"},
			QuotaCpu:    4000,
			QuotaMem:    8192,
			MaxMvmLimit: 3000,
		},
		{
			InsID:      "node-2",
			IP:         "10.0.0.2",
			CpuTotal:   8,
			MemMBTotal: 16384,
			Healthy:    false,
			NodeLabels: map[string]string{},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	got, err := loader.LoadNodes(context.Background())
	if err != nil {
		t.Fatalf("LoadNodes failed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d", len(got), len(want))
	}
	if got[0].ID() != "node-1" {
		t.Errorf("id[0]=%s, want node-1", got[0].ID())
	}
	if got[0].IP != "10.0.0.1" {
		t.Errorf("ip[0]=%s, want 10.0.0.1", got[0].IP)
	}
	if !got[0].Healthy {
		t.Errorf("healthy[0]=false, want true")
	}
	if got[0].NodeLabels["zone"] != "gz" {
		t.Errorf("labels[0][zone]=%s, want gz", got[0].NodeLabels["zone"])
	}
	if got[0].SchedulingDisabled() {
		t.Errorf("scheduling_disabled[0]=true, want false")
	}
}

func TestCubeOpsLoader_LoadNodes_NonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	_, err := loader.LoadNodes(context.Background())
	if err == nil {
		t.Fatal("expected error for non-OK response")
	}
}

func TestCubeOpsLoader_LoadNodes_Isolated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `[{"InstanceID":"node-3","IP":"10.0.0.3","SchedulingDisabled":true,"Healthy":true}]`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	got, err := loader.LoadNodes(context.Background())
	if err != nil {
		t.Fatalf("LoadNodes failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if !got[0].SchedulingDisabled() {
		t.Errorf("scheduling_disabled=false, want true")
	}
	if got[0].SchedulingAllowed() {
		t.Errorf("scheduling_allowed=true, want false for isolated node")
	}
}

func TestCubeOpsLoader_FullNodeRoundTrip(t *testing.T) {
	want := &node.Node{
		InsID:               "node-1",
		IP:                  "10.0.0.1",
		CpuTotal:            4,
		MemMBTotal:          8192,
		QuotaCpu:            4000,
		QuotaMem:            8192,
		MaxMvmLimit:         3000,
		CreateConcurrentNum: 100,
		ClusterLabel:        "gz",
		InstanceType:        "cubebox",
		HostStatus:          "running",
		ReportedReady:       true,
		Healthy:             true,
		LocalTemplates:      []string{"tpl-1", "tpl-2"},
		NodeLabels:          map[string]string{"zone": "gz"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*node.Node{want})
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	got, err := loader.LoadNodes(context.Background())
	if err != nil {
		t.Fatalf("LoadNodes failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	n := got[0]
	if n.InsID != want.InsID {
		t.Errorf("InsID=%s", n.InsID)
	}
	if n.IP != want.IP {
		t.Errorf("IP=%s", n.IP)
	}
	if n.CpuTotal != want.CpuTotal {
		t.Errorf("CpuTotal=%d", n.CpuTotal)
	}
	if n.QuotaCpu != want.QuotaCpu {
		t.Errorf("QuotaCpu=%d", n.QuotaCpu)
	}
	if n.MaxMvmLimit != want.MaxMvmLimit {
		t.Errorf("MaxMvmLimit=%d", n.MaxMvmLimit)
	}
	if len(n.LocalTemplates) != 2 || n.LocalTemplates[0] != "tpl-1" || n.LocalTemplates[1] != "tpl-2" {
		t.Errorf("LocalTemplates=%v", n.LocalTemplates)
	}
	if n.NodeLabels["zone"] != "gz" {
		t.Errorf("NodeLabels=%v", n.NodeLabels)
	}
}

func TestCubeOpsLoader_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	_, err := loader.LoadNodes(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCubeOpsLoader_NetworkTimeout(t *testing.T) {
	loader := NewCubeOpsLoader("http://127.0.0.1:1")
	loader.client.Timeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := loader.LoadNodes(ctx)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

// TestCubeOpsLoader_RetryUntilSuccess: retries after non-200 then succeeds.
func TestCubeOpsLoader_RetryUntilSuccess(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"InstanceID":"node-1","IP":"10.0.0.1","Healthy":true}]`))
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL).WithBootRetry(5, 10*time.Millisecond)
	got, err := loader.LoadNodes(context.Background())
	if err != nil {
		t.Fatalf("LoadNodes failed: %v", err)
	}
	if len(got) != 1 || got[0].InsID != "node-1" {
		t.Fatalf("unexpected nodes: %+v", got)
	}
	if calls != 3 {
		t.Errorf("expected 3 server calls, got %d", calls)
	}
}

// TestCubeOpsLoader_RetryExhausted: stops at the configured limit.
func TestCubeOpsLoader_RetryExhausted(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "still down", http.StatusInternalServerError)
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL).WithBootRetry(2, 5*time.Millisecond)
	_, err := loader.LoadNodes(context.Background())
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	// 1 initial attempt + 2 retries = 3 total calls
	if calls != 3 {
		t.Errorf("expected 3 server calls, got %d", calls)
	}
}

// TestCubeOpsLoader_RetryRespectsContext: ctx cancel aborts the loop.
func TestCubeOpsLoader_RetryRespectsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL).WithBootRetry(10, 1*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := loader.LoadNodes(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("LoadNodes ran for %s despite ctx timeout", elapsed)
	}
}

// TestCubeOpsLoader_DefaultNoRetry: legacy default makes exactly one attempt.
func TestCubeOpsLoader_DefaultNoRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	loader := NewCubeOpsLoader(server.URL)
	_, err := loader.LoadNodes(context.Background())
	if err == nil {
		t.Fatal("expected error on single attempt")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 server call, got %d", calls)
	}
}

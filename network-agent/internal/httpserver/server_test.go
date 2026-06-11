// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
)

func TestHealthHandler(t *testing.T) {
	s := New("127.0.0.1:0", service.NewNoopService())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthz status=%d, want=%d", w.Code, http.StatusOK)
	}
	if body := w.Body.String(); body != "ok" {
		t.Fatalf("healthz body=%q, want=%q", body, "ok")
	}
}

func TestEnsureHandler(t *testing.T) {
	s := New("127.0.0.1:0", service.NewNoopService())
	req := httptest.NewRequest(http.MethodPost, "/v1/network/ensure", bytes.NewBufferString(`{"sandboxID":"sb-1"}`))
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ensure status=%d, want=%d", w.Code, http.StatusOK)
	}
}

func TestNewEndpointUnix(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "network-agent.sock")
	s, err := NewEndpoint("unix://"+socketPath, service.NewNoopService())
	if err != nil {
		t.Fatalf("NewEndpoint error=%v", err)
	}
	go func() {
		_ = s.Start()
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop error=%v", err)
	}
}

// TestDumpEgressPoliciesHandlerEmpty: noop service has zero policies.
// CubeEgress's bootstrap.lua tolerates an empty policies map (it just
// loads zero rules), so this is the steady-state response for nodes
// without any L7-policied sandboxes.
func TestDumpEgressPoliciesHandlerEmpty(t *testing.T) {
	s := New("127.0.0.1:0", service.NewNoopService())
	req := httptest.NewRequest(http.MethodGet, "/v1/policies/dump", nil)
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	policies, ok := body["policies"]
	if !ok {
		t.Fatalf("response missing top-level 'policies' key (bootstrap.lua reads from there): %s", w.Body.String())
	}
	if m, _ := policies.(map[string]any); len(m) != 0 {
		t.Fatalf("noop service should produce empty policies map; got %v", m)
	}
}

// TestDumpEgressPoliciesHandlerRejectsWrongMethod: bootstrap.lua only
// does GET. Anything else should hit 405 (not 404 or 500), so an
// operator typoing curl -X POST gets a clear signal.
func TestDumpEgressPoliciesHandlerRejectsWrongMethod(t *testing.T) {
	s := New("127.0.0.1:0", service.NewNoopService())
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/dump", nil)
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", w.Code)
	}
}

// TestDumpEgressPoliciesHandlerWithFakeService: a service that returns
// a populated map should serialize to the bootstrap.lua-compatible
// shape ({"policies": {<ip>: {policy_id, rules: [...]}}}).
func TestDumpEgressPoliciesHandlerWithFakeService(t *testing.T) {
	fake := &fakeDumpService{
		policies: map[string]map[string]any{
			"192.168.0.10": {
				"policy_id": "192.168.0.10",
				"rules": []map[string]any{{
					"id":     "deepseek_api",
					"match":  map[string]any{"host": "api.deepseek.com"},
					"action": map[string]any{"allow": true},
				}},
			},
		},
	}
	s := New("127.0.0.1:0", fake)
	req := httptest.NewRequest(http.MethodGet, "/v1/policies/dump", nil)
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body struct {
		Policies map[string]map[string]any `json:"policies"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	pol, ok := body.Policies["192.168.0.10"]
	if !ok {
		t.Fatalf("missing policy for 192.168.0.10: %s", w.Body.String())
	}
	if pol["policy_id"] != "192.168.0.10" {
		t.Fatalf("policy_id=%v", pol["policy_id"])
	}
}

// fakeDumpService is a minimal Service that lets the dump-handler
// test run without a real localService (which needs a netns + cubevs
// init). Only DumpEgressPolicies is interesting; other methods are
// just enough to satisfy the interface.
type fakeDumpService struct {
	policies map[string]map[string]any
}

func (f *fakeDumpService) EnsureNetwork(ctx context.Context, req *service.EnsureNetworkRequest) (*service.EnsureNetworkResponse, error) {
	return &service.EnsureNetworkResponse{}, nil
}
func (f *fakeDumpService) ReleaseNetwork(ctx context.Context, req *service.ReleaseNetworkRequest) (*service.ReleaseNetworkResponse, error) {
	return &service.ReleaseNetworkResponse{}, nil
}
func (f *fakeDumpService) ReconcileNetwork(ctx context.Context, req *service.ReconcileNetworkRequest) (*service.ReconcileNetworkResponse, error) {
	return &service.ReconcileNetworkResponse{}, nil
}
func (f *fakeDumpService) GetNetwork(ctx context.Context, req *service.GetNetworkRequest) (*service.GetNetworkResponse, error) {
	return &service.GetNetworkResponse{}, nil
}
func (f *fakeDumpService) ListNetworks(ctx context.Context, req *service.ListNetworksRequest) (*service.ListNetworksResponse, error) {
	return &service.ListNetworksResponse{}, nil
}
func (f *fakeDumpService) Health(ctx context.Context) error { return nil }
func (f *fakeDumpService) DumpEgressPolicies(ctx context.Context) (map[string]map[string]any, error) {
	return f.policies, nil
}

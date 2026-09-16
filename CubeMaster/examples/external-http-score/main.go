// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sync"
	"time"
)

const (
	listenAddr = ":18080"

	modeMockMetrics = "mock_metrics"

	// Caps used only to normalize mock metrics into a 0-100 score.
	// These are demo constants, not production SLOs or real cluster limits.
	sandboxCountCap    = 40.0
	createLatencyCapMS = 400.0
)

type scoreRequest struct {
	Mode  string      `json:"mode,omitempty"`
	Nodes []scoreNode `json:"nodes"`
}

type scoreNode struct {
	NodeID              string  `json:"node_id"`
	RealTimeCreateNum   int64   `json:"real_time_create_num"`
	CreateConcurrentNum int64   `json:"create_concurrent_num"`
	CPUUtil             float64 `json:"cpu_util"`
	MemUsage            int64   `json:"mem_usage"`
	QuotaMem            int64   `json:"quota_mem"`
}

type scoreResponse struct {
	Scores map[string]float64 `json:"scores"`
}

// mockNodeMetrics is an in-process Prometheus-like snapshot.
// It is not scraped from Prometheus and is not a protocol field.
type mockNodeMetrics struct {
	CPUUtilization           float64 `json:"cpu_utilization"`
	MemoryUtilization        float64 `json:"memory_utilization"`
	SandboxCount             int64   `json:"sandbox_count"`
	EstimatedCreateLatencyMS float64 `json:"estimated_create_latency_ms"`
}

type mockMetricsPatch struct {
	Nodes map[string]mockNodeMetrics `json:"nodes"`
}

type mockMetricsStore struct {
	mu    sync.RWMutex
	nodes map[string]mockNodeMetrics
}

var metricsStore = newMockMetricsStore()

// faultState is explicit experiment-arm control (not random).
// POST /fault {"delay_ms":0,"http_status":0,"bad_scores":false} then GET /fault.
type faultState struct {
	mu         sync.RWMutex
	delayMS    int
	httpStatus int
	badScores  bool
}

var faults = &faultState{}

type faultPatch struct {
	DelayMS    *int  `json:"delay_ms,omitempty"`
	HTTPStatus *int  `json:"http_status,omitempty"`
	BadScores  *bool `json:"bad_scores,omitempty"`
}

func main() {
	log.Println("external_http_score demo listening on :18080")
	log.Println("modes: default/demo uses request fields; mode=mock_metrics uses in-process Prometheus-like snapshots")
	log.Println("fault arm: POST /fault {delay_ms,http_status,bad_scores}")
	log.Fatal(http.ListenAndServe(listenAddr, newHandler()))
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/score", handleScore)
	mux.HandleFunc("/mock-metrics", handleMockMetrics)
	mux.HandleFunc("/mock-metrics/reset", handleMockMetricsReset)
	mux.HandleFunc("/fault", handleFault)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (f *faultState) snapshot() map[string]any {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return map[string]any{
		"delay_ms":    f.delayMS,
		"http_status": f.httpStatus,
		"bad_scores":  f.badScores,
	}
}

func (f *faultState) apply(p faultPatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.DelayMS != nil {
		f.delayMS = *p.DelayMS
		if f.delayMS < 0 {
			f.delayMS = 0
		}
	}
	if p.HTTPStatus != nil {
		f.httpStatus = *p.HTTPStatus
	}
	if p.BadScores != nil {
		f.badScores = *p.BadScores
	}
}

func handleFault(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(faults.snapshot())
	case http.MethodPost:
		var p faultPatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		faults.apply(p)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(faults.snapshot())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleScore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	faults.mu.RLock()
	delayMS := faults.delayMS
	statusOverride := faults.httpStatus
	badScores := faults.badScores
	faults.mu.RUnlock()

	if delayMS > 0 {
		time.Sleep(time.Duration(delayMS) * time.Millisecond)
	}
	if statusOverride >= 400 {
		http.Error(w, "injected fault status", statusOverride)
		return
	}

	var req scoreRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Nodes) == 0 {
		http.Error(w, "nodes is empty", http.StatusBadRequest)
		return
	}

	rsp := scoreResponse{Scores: make(map[string]float64, len(req.Nodes))}
	if badScores {
		// Illegal for CubeMaster contract: missing candidates / wrong keys.
		rsp.Scores["no-such-node"] = 99
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rsp)
		return
	}
	for _, n := range req.Nodes {
		if n.NodeID == "" {
			http.Error(w, "node_id is required", http.StatusBadRequest)
			return
		}
		if req.Mode == modeMockMetrics {
			m, usedFallback := metricsStore.lookup(n.NodeID)
			rsp.Scores[n.NodeID] = mockMetricsScore(m)
			log.Printf("mock_metrics score node=%s cpu=%.1f mem=%.1f sandboxes=%d latency_ms=%.1f fallback=%t score=%.2f",
				n.NodeID, m.CPUUtilization, m.MemoryUtilization, m.SandboxCount, m.EstimatedCreateLatencyMS, usedFallback, rsp.Scores[n.NodeID])
			continue
		}
		rsp.Scores[n.NodeID] = demoScore(n)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rsp)
}

func handleMockMetrics(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Nodes map[string]mockNodeMetrics `json:"nodes"`
		}{Nodes: metricsStore.snapshot()})
	case http.MethodPost:
		var patch mockMetricsPatch
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(patch.Nodes) == 0 {
			http.Error(w, "nodes is empty", http.StatusBadRequest)
			return
		}
		for nodeID := range patch.Nodes {
			if nodeID == "" {
				http.Error(w, "node_id is required", http.StatusBadRequest)
				return
			}
		}
		metricsStore.replace(patch.Nodes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Nodes map[string]mockNodeMetrics `json:"nodes"`
		}{Nodes: metricsStore.snapshot()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleMockMetricsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	metricsStore.reset()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Nodes map[string]mockNodeMetrics `json:"nodes"`
	}{Nodes: metricsStore.snapshot()})
}

func demoScore(n scoreNode) float64 {
	cpuScore := clamp(100-n.CPUUtil, 0, 100)
	memScore := 100.0
	if n.QuotaMem > 0 {
		memScore = clamp(100-float64(n.MemUsage)*100/float64(n.QuotaMem), 0, 100)
	}
	createScore := 100.0
	if n.CreateConcurrentNum > 0 {
		createScore = clamp(100-float64(n.RealTimeCreateNum)*100/float64(n.CreateConcurrentNum), 0, 100)
	}
	return math.Round((cpuScore*0.5+memScore*0.3+createScore*0.2)*100) / 100
}

func mockMetricsScore(m mockNodeMetrics) float64 {
	cpuScore := clamp(100-m.CPUUtilization, 0, 100)
	memScore := clamp(100-m.MemoryUtilization, 0, 100)
	sandboxScore := clamp(100-float64(m.SandboxCount)*100/sandboxCountCap, 0, 100)
	latencyScore := clamp(100-m.EstimatedCreateLatencyMS*100/createLatencyCapMS, 0, 100)
	return math.Round((cpuScore*0.25+memScore*0.25+sandboxScore*0.25+latencyScore*0.25)*100) / 100
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func newMockMetricsStore() *mockMetricsStore {
	s := &mockMetricsStore{}
	s.reset()
	return s
}

func defaultMockMetrics() map[string]mockNodeMetrics {
	return map[string]mockNodeMetrics{
		"node-a": {
			CPUUtilization:           70,
			MemoryUtilization:        60,
			SandboxCount:             20,
			EstimatedCreateLatencyMS: 200,
		},
		"node-b": {
			CPUUtilization:           20,
			MemoryUtilization:        30,
			SandboxCount:             5,
			EstimatedCreateLatencyMS: 50,
		},
	}
}

func fallbackMockMetrics() mockNodeMetrics {
	return mockNodeMetrics{
		CPUUtilization:           50,
		MemoryUtilization:        50,
		SandboxCount:             10,
		EstimatedCreateLatencyMS: 100,
	}
}

func (s *mockMetricsStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = defaultMockMetrics()
}

func (s *mockMetricsStore) snapshot() map[string]mockNodeMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]mockNodeMetrics, len(s.nodes))
	for id, m := range s.nodes {
		out[id] = m
	}
	return out
}

func (s *mockMetricsStore) replace(nodes map[string]mockNodeMetrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range nodes {
		s.nodes[id] = m
	}
}

func (s *mockMetricsStore) lookup(nodeID string) (mockNodeMetrics, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.nodes[nodeID]; ok {
		return m, false
	}
	return fallbackMockMetrics(), true
}

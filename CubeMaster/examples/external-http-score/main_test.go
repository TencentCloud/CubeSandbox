// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	defaultMockScoreNodeA = 42.5
	defaultMockScoreNodeB = 81.25
	flippedMockScoreNodeA = 90.0
	flippedMockScoreNodeB = 31.25

	demoRequestScoreNodeA = 76.83
	demoRequestScoreNodeB = 88.75
)

func TestDefaultModeScoresFromRequestFields(t *testing.T) {
	srv := newTestServer(t)

	patchFlippedMockMetrics(t, srv)

	for _, mode := range []string{"", "default", "demo"} {
		t.Run("mode="+mode, func(t *testing.T) {
			scores := postScore(t, srv, scoreRequest{
				Mode:  mode,
				Nodes: demoRequestNodes(),
			})
			assertExactScores(t, scores, map[string]float64{
				"node-a": demoRequestScoreNodeA,
				"node-b": demoRequestScoreNodeB,
			})
			if scores["node-b"] <= scores["node-a"] {
				t.Fatalf("default/demo mode node-b=%v, want greater than node-a=%v", scores["node-b"], scores["node-a"])
			}
		})
	}
}

func TestMockMetricsModeUsesDefaultSnapshot(t *testing.T) {
	srv := newTestServer(t)

	scores := postScore(t, srv, mockMetricsScoreRequest())
	assertExactScores(t, scores, map[string]float64{
		"node-a": defaultMockScoreNodeA,
		"node-b": defaultMockScoreNodeB,
	})
	if scores["node-b"] <= scores["node-a"] {
		t.Fatalf("default mock metrics node-b=%v, want greater than node-a=%v", scores["node-b"], scores["node-a"])
	}
}

func TestMockMetricsPatchFlipsNodePreference(t *testing.T) {
	srv := newTestServer(t)

	before := postScore(t, srv, mockMetricsScoreRequest())
	assertExactScores(t, before, map[string]float64{
		"node-a": defaultMockScoreNodeA,
		"node-b": defaultMockScoreNodeB,
	})
	if before["node-b"] <= before["node-a"] {
		t.Fatalf("before patch node-b=%v, want greater than node-a=%v", before["node-b"], before["node-a"])
	}

	patchFlippedMockMetrics(t, srv)

	after := postScore(t, srv, mockMetricsScoreRequest())
	assertExactScores(t, after, map[string]float64{
		"node-a": flippedMockScoreNodeA,
		"node-b": flippedMockScoreNodeB,
	})
	if after["node-a"] <= after["node-b"] {
		t.Fatalf("after patch node-a=%v, want greater than node-b=%v", after["node-a"], after["node-b"])
	}
	if after["node-a"] == before["node-a"] || after["node-b"] == before["node-b"] {
		t.Fatalf("after patch scores = %v, want both node scores to change from %v", after, before)
	}
}

func TestMockMetricsResetRestoresDefaultSnapshot(t *testing.T) {
	srv := newTestServer(t)

	patchFlippedMockMetrics(t, srv)
	flipped := postScore(t, srv, mockMetricsScoreRequest())
	assertExactScores(t, flipped, map[string]float64{
		"node-a": flippedMockScoreNodeA,
		"node-b": flippedMockScoreNodeB,
	})

	status, body := doJSON(t, srv, http.MethodPost, "/mock-metrics/reset", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /mock-metrics/reset status = %d, want 200; body = %s", status, body)
	}

	gotSnapshot := decodeMockMetrics(t, body)
	wantSnapshot := defaultMockMetrics()
	if len(gotSnapshot) != len(wantSnapshot) {
		t.Fatalf("reset snapshot nodes = %d, want %d", len(gotSnapshot), len(wantSnapshot))
	}
	for id, want := range wantSnapshot {
		got, ok := gotSnapshot[id]
		if !ok {
			t.Fatalf("reset snapshot missing %s", id)
		}
		if got != want {
			t.Fatalf("reset snapshot %s = %+v, want %+v", id, got, want)
		}
	}

	restored := postScore(t, srv, mockMetricsScoreRequest())
	assertExactScores(t, restored, map[string]float64{
		"node-a": defaultMockScoreNodeA,
		"node-b": defaultMockScoreNodeB,
	})
	if restored["node-b"] <= restored["node-a"] {
		t.Fatalf("after reset node-b=%v, want greater than node-a=%v", restored["node-b"], restored["node-a"])
	}
}

func TestScoreResponseMatchesExternalHTTPScoreProtocol(t *testing.T) {
	srv := newTestServer(t)

	cases := []scoreRequest{
		{Mode: "default", Nodes: demoRequestNodes()},
		mockMetricsScoreRequest(),
	}
	for _, req := range cases {
		body := postScoreRaw(t, srv, req)
		scores := decodeProtocolScores(t, body)
		if len(scores) != len(req.Nodes) {
			t.Fatalf("mode=%q scores count = %d, want %d", req.Mode, len(scores), len(req.Nodes))
		}
		for _, n := range req.Nodes {
			score, ok := scores[n.NodeID]
			if !ok {
				t.Fatalf("mode=%q missing score for %s", req.Mode, n.NodeID)
			}
			assertScoreInProtocolRange(t, n.NodeID, score)
		}
	}
}

func TestScoreRejectsEmptyNodes(t *testing.T) {
	srv := newTestServer(t)

	status, body := doJSON(t, srv, http.MethodPost, "/score", scoreRequest{Nodes: nil})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /score empty nodes status = %d, want 400; body = %s", status, body)
	}
	if !strings.Contains(string(body), "nodes is empty") {
		t.Fatalf("POST /score empty nodes body = %q, want nodes is empty", body)
	}
}

func TestScoreRejectsMissingNodeID(t *testing.T) {
	srv := newTestServer(t)

	status, body := doJSON(t, srv, http.MethodPost, "/score", scoreRequest{
		Nodes: []scoreNode{
			{NodeID: "node-a", CPUUtil: 30},
			{CPUUtil: 15},
		},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /score missing node_id status = %d, want 400; body = %s", status, body)
	}
	if !strings.Contains(string(body), "node_id is required") {
		t.Fatalf("POST /score missing node_id body = %q, want node_id is required", body)
	}
}

func TestMockMetricsRejectsEmptyNodes(t *testing.T) {
	srv := newTestServer(t)

	status, body := doJSON(t, srv, http.MethodPost, "/mock-metrics", mockMetricsPatch{Nodes: map[string]mockNodeMetrics{}})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /mock-metrics empty nodes status = %d, want 400; body = %s", status, body)
	}
	if !strings.Contains(string(body), "nodes is empty") {
		t.Fatalf("POST /mock-metrics empty nodes body = %q, want nodes is empty", body)
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	metricsStore.reset()
	faults.apply(faultPatch{
		DelayMS:    intPtr(0),
		HTTPStatus: intPtr(0),
		BadScores:  boolPtr(false),
	})
	t.Cleanup(func() {
		metricsStore.reset()
		faults.apply(faultPatch{
			DelayMS:    intPtr(0),
			HTTPStatus: intPtr(0),
			BadScores:  boolPtr(false),
		})
	})

	srv := httptest.NewServer(newHandler())
	t.Cleanup(srv.Close)
	return srv
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

func TestFaultBadScoresReturnsWrongKeys(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal(faultPatch{BadScores: boolPtr(true)})
	resp, err := http.Post(srv.URL+"/fault", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fault status=%d", resp.StatusCode)
	}
	scores := postScore(t, srv, scoreRequest{Nodes: demoRequestNodes()})
	if _, ok := scores["no-such-node"]; !ok {
		t.Fatalf("want no-such-node key, got %#v", scores)
	}
	if _, ok := scores["node-a"]; ok {
		t.Fatalf("did not expect node-a when bad_scores injected")
	}
}

func TestFaultHTTPStatus(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal(faultPatch{HTTPStatus: intPtr(503)})
	resp, err := http.Post(srv.URL+"/fault", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	reqBody, _ := json.Marshal(scoreRequest{Nodes: demoRequestNodes()})
	scoreResp, err := http.Post(srv.URL+"/score", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer scoreResp.Body.Close()
	if scoreResp.StatusCode != 503 {
		t.Fatalf("want 503 got %d", scoreResp.StatusCode)
	}
}

func demoRequestNodes() []scoreNode {
	return []scoreNode{
		{
			NodeID:              "node-a",
			CPUUtil:             30,
			MemUsage:            32768,
			QuotaMem:            131072,
			RealTimeCreateNum:   1,
			CreateConcurrentNum: 30,
		},
		{
			NodeID:              "node-b",
			CPUUtil:             15,
			MemUsage:            16384,
			QuotaMem:            131072,
			RealTimeCreateNum:   0,
			CreateConcurrentNum: 30,
		},
	}
}

func mockMetricsScoreRequest() scoreRequest {
	return scoreRequest{
		Mode: modeMockMetrics,
		Nodes: []scoreNode{
			{NodeID: "node-a"},
			{NodeID: "node-b"},
		},
	}
}

func flippedMockMetrics() map[string]mockNodeMetrics {
	return map[string]mockNodeMetrics{
		"node-a": {
			CPUUtilization:           10,
			MemoryUtilization:        15,
			SandboxCount:             2,
			EstimatedCreateLatencyMS: 40,
		},
		"node-b": {
			CPUUtilization:           80,
			MemoryUtilization:        75,
			SandboxCount:             24,
			EstimatedCreateLatencyMS: 240,
		},
	}
}

func patchFlippedMockMetrics(t *testing.T, srv *httptest.Server) {
	t.Helper()
	status, body := doJSON(t, srv, http.MethodPost, "/mock-metrics", mockMetricsPatch{Nodes: flippedMockMetrics()})
	if status != http.StatusOK {
		t.Fatalf("POST /mock-metrics status = %d, want 200; body = %s", status, body)
	}
}

func postScore(t *testing.T, srv *httptest.Server, req scoreRequest) map[string]float64 {
	t.Helper()
	return decodeProtocolScores(t, postScoreRaw(t, srv, req))
}

func postScoreRaw(t *testing.T, srv *httptest.Server, req scoreRequest) []byte {
	t.Helper()
	status, body := doJSON(t, srv, http.MethodPost, "/score", req)
	if status != http.StatusOK {
		t.Fatalf("POST /score status = %d, want 200; body = %s", status, body)
	}
	return body
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, payload any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

func decodeProtocolScores(t *testing.T, body []byte) map[string]float64 {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode score response: %v; body = %s", err, body)
	}
	if len(raw) != 1 {
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		t.Fatalf("score response keys = %v, want only scores", keys)
	}
	scoresRaw, ok := raw["scores"]
	if !ok {
		t.Fatalf("score response missing scores; body = %s", body)
	}
	var scores map[string]float64
	if err := json.Unmarshal(scoresRaw, &scores); err != nil {
		t.Fatalf("decode scores map: %v; body = %s", err, body)
	}
	if len(scores) == 0 {
		t.Fatal("scores map is empty")
	}
	for nodeID, score := range scores {
		assertScoreInProtocolRange(t, nodeID, score)
	}
	return scores
}

func decodeMockMetrics(t *testing.T, body []byte) map[string]mockNodeMetrics {
	t.Helper()
	var out struct {
		Nodes map[string]mockNodeMetrics `json:"nodes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode mock metrics: %v; body = %s", err, body)
	}
	return out.Nodes
}

func assertExactScores(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("scores = %v, want %v", got, want)
	}
	for id, wantScore := range want {
		gotScore, ok := got[id]
		if !ok {
			t.Fatalf("scores missing %s; got %v, want %v", id, got, want)
		}
		if gotScore != wantScore {
			t.Fatalf("score[%s] = %v, want %v", id, gotScore, wantScore)
		}
	}
}

func assertScoreInProtocolRange(t *testing.T, nodeID string, score float64) {
	t.Helper()
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 100 {
		t.Fatalf("invalid protocol score for %s: %v, want finite value in [0, 100]", nodeID, score)
	}
}

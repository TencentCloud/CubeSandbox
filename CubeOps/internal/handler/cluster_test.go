// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func newClusterRouter(t *testing.T, cm CubeMasterClient) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewClusterHandler(cm)
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

func TestCluster_Overview_Success(t *testing.T) {
	cm := &fakeCM{
		getNodes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{
				"data": [
					{"node_id": "n-1", "host_ip": "10.0.0.1", "instance_type": "cubebox",
					 "healthy": true, "capacity": {"milli_cpu": 4000, "memory_mb": 8192},
					 "allocatable": {"milli_cpu": 2000, "memory_mb": 4096},
					 "max_mvm_num": 10, "quota_cpu": 4000, "quota_mem_mb": 8192,
					 "create_concurrent_num": 5, "conditions": [], "local_templates": [], "versions": []},
					{"node_id": "n-2", "host_ip": "10.0.0.2", "instance_type": "cubebox",
					 "healthy": false, "capacity": {"milli_cpu": 2000, "memory_mb": 4096},
					 "allocatable": {"milli_cpu": 2000, "memory_mb": 4096},
					 "max_mvm_num": 5, "quota_cpu": 2000, "quota_mem_mb": 4096,
					 "create_concurrent_num": 3, "conditions": [], "local_templates": [], "versions": []}
				]
			}`), nil
		},
		// fetchUsedResources calls ListSandboxes; return empty to fall back to
		// allocatable diff.
		listSandboxes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": []}`), nil
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/overview")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var ov map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if ov["nodeCount"] != float64(2) {
		t.Errorf("nodeCount = %v, want 2", ov["nodeCount"])
	}
	if ov["healthyNodes"] != float64(1) {
		t.Errorf("healthyNodes = %v, want 1", ov["healthyNodes"])
	}
	if ov["totalCpuMilli"] != float64(6000) {
		t.Errorf("totalCpuMilli = %v, want 6000", ov["totalCpuMilli"])
	}
	if ov["maxMvmSlots"] != float64(15) {
		t.Errorf("maxMvmSlots = %v, want 15", ov["maxMvmSlots"])
	}
}

func TestCluster_Overview_CMError(t *testing.T) {
	cm := &fakeCM{
		getNodes: func(_ context.Context) (json.RawMessage, error) {
			return nil, errFakeNotConfigured
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/overview")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestCluster_ListNodes_Success(t *testing.T) {
	cm := &fakeCM{
		getNodes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": [
				{"node_id": "n-1", "host_ip": "10.0.0.1", "instance_type": "cubebox",
				 "healthy": true, "capacity": {"milli_cpu": 4000, "memory_mb": 8192},
				 "allocatable": {"milli_cpu": 4000, "memory_mb": 8192},
				 "max_mvm_num": 10, "quota_cpu": 4000, "quota_mem_mb": 8192,
				 "create_concurrent_num": 5, "conditions": [], "local_templates": [], "versions": []}
			]}`), nil
		},
		listSandboxes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": []}`), nil
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/nodes")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var nodes []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0]["nodeID"] != "n-1" {
		t.Errorf("nodeID = %v, want n-1", nodes[0]["nodeID"])
	}
	if nodes[0]["healthy"] != true {
		t.Errorf("healthy = %v, want true", nodes[0]["healthy"])
	}
	// snake_case → camelCase for nested fields.
	cap, _ := nodes[0]["capacity"].(map[string]interface{})
	if cap["cpuMilli"] != float64(4000) {
		t.Errorf("capacity.cpuMilli = %v, want 4000", cap["cpuMilli"])
	}
}

func TestCluster_GetNode_NotFound(t *testing.T) {
	cm := &fakeCM{
		getNode: func(_ context.Context, _ string) (json.RawMessage, error) {
			return raw(`{"data": null}`), nil
		},
		listSandboxes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": []}`), nil
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/nodes/ghost")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestCluster_Versions_PassThrough(t *testing.T) {
	cm := &fakeCM{
		clusterVersions: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": {"control_plane": "v1.0"}}`), nil
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The data field is passed through verbatim.
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp["control_plane"] != "v1.0" {
		t.Errorf("control_plane = %v, want v1.0", resp["control_plane"])
	}
}

func TestCluster_Versions_CMError_ReturnsEmptyShell(t *testing.T) {
	cm := &fakeCM{
		clusterVersions: func(_ context.Context) (json.RawMessage, error) {
			return nil, errFakeNotConfigured
		},
	}
	r := newClusterRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Versions returns empty shell on CM error)", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Should return an empty structure, not an error, so the UI doesn't break.
	if _, ok := resp["controlPlane"]; !ok {
		t.Errorf("expected controlPlane key in empty shell, got %v", resp)
	}
}

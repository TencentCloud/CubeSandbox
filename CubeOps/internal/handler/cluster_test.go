// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	nmhandler "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// fakeClusterNodeService is a stub nmhandler.NodeService for cluster handler tests.
type fakeClusterNodeService struct {
	listNodes        func(ctx context.Context) ([]*model.NodeSnapshot, error)
	getNode          func(ctx context.Context, nodeID string) (*model.NodeSnapshot, error)
	getVersionMatrix func(ctx context.Context) (*model.VersionMatrix, error)
}

func (f *fakeClusterNodeService) RegisterNode(ctx context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClusterNodeService) UpdateNodeStatus(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClusterNodeService) GetNode(ctx context.Context, nodeID string) (*model.NodeSnapshot, error) {
	return f.getNode(ctx, nodeID)
}
func (f *fakeClusterNodeService) ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error) {
	return f.listNodes(ctx)
}
func (f *fakeClusterNodeService) UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error {
	return errors.New("not implemented")
}
func (f *fakeClusterNodeService) DeleteNodeLabel(ctx context.Context, nodeID, key, operator string) error {
	return errors.New("not implemented")
}
func (f *fakeClusterNodeService) SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClusterNodeService) GetVersionMatrix(ctx context.Context) (*model.VersionMatrix, error) {
	return f.getVersionMatrix(ctx)
}
func (f *fakeClusterNodeService) ListOperations(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClusterNodeService) DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error) {
	return nil, errors.New("not implemented")
}

func newClusterRouter(t *testing.T, cm CubeMasterClient, svc nmhandler.NodeService) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewClusterHandler(cm).WithNodeService(svc)
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

func testNodes() []*model.NodeSnapshot {
	return []*model.NodeSnapshot{
		{NodeID: "n-1", HostIP: "10.0.0.1", InstanceType: "cubebox", Healthy: true,
			Capacity: model.ResourceSnapshot{MilliCPU: 4000, MemoryMB: 8192}, MaxMvmNum: 10},
		{NodeID: "n-2", HostIP: "10.0.0.2", InstanceType: "cubebox", Healthy: false,
			Capacity: model.ResourceSnapshot{MilliCPU: 2000, MemoryMB: 4096}, MaxMvmNum: 5},
	}
}

func TestCluster_Overview_Success(t *testing.T) {
	cm := &fakeCM{
		listSandboxes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": []}`), nil
		},
	}
	svc := &fakeClusterNodeService{
		listNodes: func(_ context.Context) ([]*model.NodeSnapshot, error) {
			return testNodes(), nil
		},
	}
	r := newClusterRouter(t, cm, svc)

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
}

func TestCluster_Overview_ServiceError(t *testing.T) {
	cm := &fakeCM{}
	svc := &fakeClusterNodeService{
		listNodes: func(_ context.Context) ([]*model.NodeSnapshot, error) {
			return nil, errors.New("db down")
		},
	}
	r := newClusterRouter(t, cm, svc)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/overview")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestCluster_ListNodes_Success(t *testing.T) {
	cm := &fakeCM{
		listSandboxes: func(_ context.Context) (json.RawMessage, error) {
			return raw(`{"data": []}`), nil
		},
	}
	svc := &fakeClusterNodeService{
		listNodes: func(_ context.Context) ([]*model.NodeSnapshot, error) {
			return testNodes(), nil
		},
	}
	r := newClusterRouter(t, cm, svc)

	w := httptestRecorder(t, r, "GET", "/api/v1/nodes")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var nodes []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(nodes) != 2 {
		t.Errorf("len(nodes) = %d, want 2", len(nodes))
	}
	if nodes[0]["nodeID"] != "n-1" {
		t.Errorf("nodes[0].nodeID = %v, want n-1", nodes[0]["nodeID"])
	}
}

func TestCluster_GetNode_NotFound(t *testing.T) {
	cm := &fakeCM{}
	svc := &fakeClusterNodeService{
		getNode: func(_ context.Context, _ string) (*model.NodeSnapshot, error) {
			return nil, errors.New("not found")
		},
	}
	r := newClusterRouter(t, cm, svc)

	w := httptestRecorder(t, r, "GET", "/api/v1/nodes/ghost")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestCluster_Versions_Success(t *testing.T) {
	cm := &fakeCM{}
	svc := &fakeClusterNodeService{
		getVersionMatrix: func(_ context.Context) (*model.VersionMatrix, error) {
			return &model.VersionMatrix{
				ControlPlane: map[string]string{"cubemaster": "v0.6.0"},
			}, nil
		},
	}
	r := newClusterRouter(t, cm, svc)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	cp, ok := resp["control_plane"].(map[string]interface{})
	if !ok || cp["cubemaster"] != "v0.6.0" {
		t.Errorf("unexpected versions response: %v", resp)
	}
}

func TestCluster_Versions_Error_ReturnsEmptyShell(t *testing.T) {
	cm := &fakeCM{}
	svc := &fakeClusterNodeService{
		getVersionMatrix: func(_ context.Context) (*model.VersionMatrix, error) {
			return nil, errors.New("db down")
		},
	}
	r := newClusterRouter(t, cm, svc)

	w := httptestRecorder(t, r, "GET", "/api/v1/cluster/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Versions returns empty shell on error)", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if _, ok := resp["controlPlane"]; !ok {
		t.Errorf("expected controlPlane key in empty shell: %v", resp)
	}
}

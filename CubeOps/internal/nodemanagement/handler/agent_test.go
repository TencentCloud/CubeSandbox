// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func init() { gin.SetMode(gin.TestMode) }

type fakeNodeService struct {
	registerNode              func(ctx context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error)
	updateNodeStatus          func(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error)
	getNode                   func(ctx context.Context, nodeID string) (*model.NodeSnapshot, error)
	listNodes                 func(ctx context.Context) ([]*model.NodeSnapshot, error)
	updateNodeLabels          func(ctx context.Context, nodeID string, labels map[string]string, operator string) error
	deleteNodeLabel           func(ctx context.Context, nodeID, key, operator string) error
	setNodeSchedulingDisabled func(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error)
	getVersionMatrix          func(ctx context.Context) (*model.VersionMatrix, error)
	listOperations            func(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error)
	deleteNode                func(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error)
	getNodeQuotaView          func(ctx context.Context, nodeID string) (*model.QuotaView, error)
	setNodeQuota              func(ctx context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error)
	getOpsAgentSpec           func(ctx context.Context, nodeID string) (*model.OpsAgentSpecResponse, error)
	getClusterQuotaDefaults   func(ctx context.Context) (*model.ClusterQuotaDefaults, error)
	setClusterQuotaDefaults   func(ctx context.Context, ratio *float64, operator string) (*model.ClusterQuotaDefaults, *model.QuotaPropagation, error)
}

func (f *fakeNodeService) RegisterNode(ctx context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error) {
	return f.registerNode(ctx, req)
}
func (f *fakeNodeService) UpdateNodeStatus(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
	return f.updateNodeStatus(ctx, nodeID, req)
}
func (f *fakeNodeService) GetNode(ctx context.Context, nodeID string) (*model.NodeSnapshot, error) {
	return f.getNode(ctx, nodeID)
}
func (f *fakeNodeService) ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error) {
	return f.listNodes(ctx)
}
func (f *fakeNodeService) UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error {
	return f.updateNodeLabels(ctx, nodeID, labels, operator)
}
func (f *fakeNodeService) DeleteNodeLabel(ctx context.Context, nodeID, key, operator string) error {
	return f.deleteNodeLabel(ctx, nodeID, key, operator)
}
func (f *fakeNodeService) SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error) {
	return f.setNodeSchedulingDisabled(ctx, nodeID, disabled, operator, detail)
}
func (f *fakeNodeService) GetVersionMatrix(ctx context.Context) (*model.VersionMatrix, error) {
	return f.getVersionMatrix(ctx)
}
func (f *fakeNodeService) ListOperations(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error) {
	return f.listOperations(ctx, nodeID, limit)
}
func (f *fakeNodeService) DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error) {
	if f.deleteNode == nil {
		return nil, errors.New("not implemented")
	}
	return f.deleteNode(ctx, nodeID, force)
}
func (f *fakeNodeService) GetNodeQuotaView(ctx context.Context, nodeID string) (*model.QuotaView, error) {
	return f.getNodeQuotaView(ctx, nodeID)
}
func (f *fakeNodeService) SetNodeQuota(ctx context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
	return f.setNodeQuota(ctx, nodeID, spec, operator)
}
func (f *fakeNodeService) GetOpsAgentSpec(ctx context.Context, nodeID string) (*model.OpsAgentSpecResponse, error) {
	return f.getOpsAgentSpec(ctx, nodeID)
}
func (f *fakeNodeService) GetClusterQuotaDefaults(ctx context.Context) (*model.ClusterQuotaDefaults, error) {
	if f.getClusterQuotaDefaults == nil {
		return &model.ClusterQuotaDefaults{}, nil
	}
	return f.getClusterQuotaDefaults(ctx)
}
func (f *fakeNodeService) SetClusterQuotaDefaults(ctx context.Context, ratio *float64, operator string) (*model.ClusterQuotaDefaults, *model.QuotaPropagation, error) {
	if f.setClusterQuotaDefaults == nil {
		return &model.ClusterQuotaDefaults{PausedReleaseRatio: ratio}, &model.QuotaPropagation{}, nil
	}
	return f.setClusterQuotaDefaults(ctx, ratio, operator)
}

func TestAgent_RegisterNode(t *testing.T) {
	svc := &fakeNodeService{
		registerNode: func(_ context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error) {
			return &model.NodeSnapshot{NodeID: req.NodeID, HostIP: req.HostIP}, nil
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.RegisterNodeRequest{NodeID: "n-1", HostIP: "10.0.0.1"})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp model.NodeSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NodeID != "n-1" {
		t.Errorf("nodeID = %s", resp.NodeID)
	}
}

func TestAgent_Readyz(t *testing.T) {
	r := gin.New()
	handler.NewAgentHandler(&fakeNodeService{}).Register(r.Group("/internal/v1/node-agent"))

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/node-agent/readyz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Ret struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Ret.RetCode != 200 {
		t.Errorf("ret_code = %d, want 200", resp.Ret.RetCode)
	}
}

func TestAgent_UpdateStatus(t *testing.T) {
	svc := &fakeNodeService{
		updateNodeStatus: func(_ context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
			return &model.NodeSnapshot{NodeID: nodeID, Healthy: true, HeartbeatTime: time.Now()}, nil
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.UpdateNodeStatusRequest{Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}}})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestAgent_UpdateStatus_WithAllocated(t *testing.T) {
	var capturedNodeID string
	var capturedReq *model.UpdateNodeStatusRequest
	svc := &fakeNodeService{
		updateNodeStatus: func(_ context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
			capturedNodeID = nodeID
			capturedReq = req
			return &model.NodeSnapshot{NodeID: nodeID, Healthy: true, HeartbeatTime: time.Now()}, nil
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.UpdateNodeStatusRequest{
		Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
		Allocated:  &model.AllocatedResources{MilliCPU: 1000, MemoryMB: 2048, MvmNum: 3},
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if capturedNodeID != "n-1" {
		t.Errorf("nodeID = %s", capturedNodeID)
	}
	if capturedReq == nil || capturedReq.Allocated == nil {
		t.Fatal("expected allocated request")
	}
	if capturedReq.Allocated.MilliCPU != 1000 || capturedReq.Allocated.MvmNum != 3 {
		t.Errorf("allocated = %+v", capturedReq.Allocated)
	}
}

// The heartbeat quota JSON keys must match cubelet's masterclient wire format.
func TestAgent_UpdateStatus_WithQuota(t *testing.T) {
	var capturedReq *model.UpdateNodeStatusRequest
	svc := &fakeNodeService{
		updateNodeStatus: func(_ context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
			capturedReq = req
			return &model.NodeSnapshot{NodeID: nodeID, Healthy: true, HeartbeatTime: time.Now()}, nil
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	// Raw payload mirrors cubelet's masterclient JSON keys, not the Go struct.
	body := []byte(`{
		"conditions": [{"type": "Ready", "status": "True"}],
		"quota": {"milli_cpu": 128000, "mem_mb": 262144, "max_mvm_num": 500, "create_concurrent_num": 32}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if capturedReq == nil || capturedReq.Quota == nil {
		t.Fatal("expected quota in captured request")
	}
	q := capturedReq.Quota
	if q.MilliCPU != 128000 || q.MemMB != 262144 || q.MaxMvmNum != 500 || q.CreateConcurrentNum != 32 {
		t.Errorf("quota = %+v", q)
	}
}

func TestAgent_UpdateStatus_WithLocalTemplates(t *testing.T) {
	var capturedReq *model.UpdateNodeStatusRequest
	svc := &fakeNodeService{
		updateNodeStatus: func(_ context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
			capturedReq = req
			return &model.NodeSnapshot{NodeID: nodeID, Healthy: true, HeartbeatTime: time.Now()}, nil
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.UpdateNodeStatusRequest{
		Conditions:     []model.NodeCondition{{Type: "Ready", Status: "True"}},
		LocalTemplates: []model.LocalTemplate{{TemplateID: "tpl-1"}, {TemplateID: "tpl-2"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if capturedReq == nil || len(capturedReq.LocalTemplates) != 2 {
		t.Fatalf("local templates = %+v", capturedReq.LocalTemplates)
	}
	if capturedReq.LocalTemplates[0].TemplateID != "tpl-1" {
		t.Errorf("template[0] = %s", capturedReq.LocalTemplates[0].TemplateID)
	}
}

func TestAgent_UpdateStatus_BadRequestOnNilBody(t *testing.T) {
	svc := &fakeNodeService{}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAgent_UpdateStatus_UnregisteredNode(t *testing.T) {
	svc := &fakeNodeService{
		updateNodeStatus: func(_ context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
			return nil, store.ErrNotFound
		},
	}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.UpdateNodeStatusRequest{Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}}})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes/n-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAgent_UpdateStatus_MissingNodeID(t *testing.T) {
	svc := &fakeNodeService{}
	r := gin.New()
	handler.NewAgentHandler(svc).Register(r.Group("/internal/v1/node-agent"))

	body, _ := json.Marshal(model.UpdateNodeStatusRequest{Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}}})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/node-agent/nodes//status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

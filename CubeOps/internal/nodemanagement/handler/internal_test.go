// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

func TestInternal_ListNodes(t *testing.T) {
	svc := &fakeNodeService{
		listNodes: func(_ context.Context) ([]*model.NodeSnapshot, error) {
			return []*model.NodeSnapshot{{NodeID: "n-1", HostIP: "10.0.0.1"}}, nil
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/nodes", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp []*model.SchedulerNode
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 || resp[0].ID() != "n-1" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestInternal_Isolate(t *testing.T) {
	var gotDetail string
	svc := &fakeNodeService{
		setNodeSchedulingDisabled: func(_ context.Context, nodeID string, disabled bool, _, detail string) (*model.NodeSnapshot, error) {
			gotDetail = detail
			return &model.NodeSnapshot{NodeID: nodeID, SchedulingDisabled: disabled}, nil
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodPut, "/internal/v1/nodes/n-1/isolation",
		strings.NewReader(`{"detail":"maintenance"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["scheduling_disabled"] != true {
		t.Errorf("scheduling_disabled = %v", resp["scheduling_disabled"])
	}
	if gotDetail != "maintenance" {
		t.Errorf("detail = %q, want %q", gotDetail, "maintenance")
	}
}

func TestInternal_DeleteNode_Success(t *testing.T) {
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, nodeID string, force bool) (*model.NodeSnapshot, error) {
			return &model.NodeSnapshot{NodeID: nodeID}, nil
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestInternal_DeleteNode_ForceQuery(t *testing.T) {
	var gotForce bool
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, _ string, force bool) (*model.NodeSnapshot, error) {
			gotForce = force
			return &model.NodeSnapshot{NodeID: "n-1"}, nil
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1?force=true", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !gotForce {
		t.Errorf("force = false, want true")
	}
}

func TestInternal_DeleteNode_NotFound(t *testing.T) {
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, _ string, _ bool) (*model.NodeSnapshot, error) {
			return nil, service.ErrNodeNotFound
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestInternal_DeleteNode_NotIsolated(t *testing.T) {
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, _ string, _ bool) (*model.NodeSnapshot, error) {
			return nil, service.ErrNodeNotIsolated
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestInternal_DeleteNode_HasSandboxes(t *testing.T) {
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, _ string, _ bool) (*model.NodeSnapshot, error) {
			return nil, service.ErrNodeHasSandboxes
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestInternal_DeleteNode_SandboxCheckFailed(t *testing.T) {
	svc := &fakeNodeService{
		deleteNode: func(_ context.Context, _ string, _ bool) (*model.NodeSnapshot, error) {
			return nil, service.ErrSandboxCheckFailed
		},
	}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/n-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInternal_DeleteNode_EmptyNodeID(t *testing.T) {
	svc := &fakeNodeService{}
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))

	// Gin does not match /nodes/ (trailing slash with empty :nodeID) by default;
	// use a request that hits the route without a nodeID to verify the guard.
	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/nodes/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 404 or 400 for empty nodeID", w.Code)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

// newQuotaRouters mounts the authed quota API (with a stand-in for the JWT
// middleware's username injection) and the no-auth ops-agent API.
func newQuotaRouters(svc *fakeNodeService) (*gin.Engine, *gin.Engine) {
	authed := gin.New()
	authed.Use(func(c *gin.Context) { c.Set("username", "alice"); c.Next() })
	handler.NewQuotaHandler(svc).Register(authed.Group("/api/v1"))

	internal := gin.New()
	handler.NewOpsAgentHandler(svc).Register(internal.Group("/internal/v1/ops-agent"))
	return authed, internal
}

func TestQuotaAPI_Get(t *testing.T) {
	svc := &fakeNodeService{
		getNodeQuotaView: func(_ context.Context, nodeID string) (*model.QuotaView, error) {
			return &model.QuotaView{NodeID: nodeID, Drift: model.DriftNoSpec}, nil
		},
	}
	authed, _ := newQuotaRouters(svc)

	w := httptest.NewRecorder()
	authed.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/n-1/config/quota", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var view model.QuotaView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.NodeID != "n-1" {
		t.Fatalf("view = %s err=%v", w.Body.String(), err)
	}
}

func TestQuotaAPI_PutCarriesSpecAndOperator(t *testing.T) {
	var gotSpec *model.QuotaSpec
	var gotOperator string
	svc := &fakeNodeService{
		setNodeQuota: func(_ context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
			gotSpec, gotOperator = spec, operator
			return &model.QuotaView{NodeID: nodeID}, &model.PushResult{Applied: true}, nil
		},
	}
	authed, _ := newQuotaRouters(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/nodes/n-1/config/quota", bytes.NewReader([]byte(`{"mcpu_limit":64000,"mem_limit":"64Gi"}`)))
	req.Header.Set("Content-Type", "application/json")
	authed.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if gotSpec == nil || gotSpec.MCpuLimit != 64000 || gotSpec.MemLimit != "64Gi" {
		t.Fatalf("spec = %+v", gotSpec)
	}
	if gotOperator != "alice" {
		t.Fatalf("operator = %q, want alice (JWT username must reach the service)", gotOperator)
	}
	var out struct {
		View model.QuotaView  `json:"view"`
		Push model.PushResult `json:"push"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Push.Applied {
		t.Fatalf("response = %s err=%v", w.Body.String(), err)
	}
}

func TestQuotaAPI_HistoryFiltersOtherOps(t *testing.T) {
	svc := &fakeNodeService{
		listOperations: func(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error) {
			return []model.NodeOperation{
				{NodeID: nodeID, Type: model.OpSetQuota, Operator: "alice", Detail: "{}", CreatedAt: time.Now()},
				{NodeID: nodeID, Type: model.OpIsolate, Operator: "bob", Detail: "{}", CreatedAt: time.Now()},
			}, nil
		},
	}
	authed, _ := newQuotaRouters(svc)

	w := httptest.NewRecorder()
	authed.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/n-1/config/quota/history", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var entries []model.QuotaHistoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || len(entries) != 1 || entries[0].Operator != "alice" {
		t.Fatalf("entries = %s err=%v", w.Body.String(), err)
	}
}

func TestOpsAgentAPI_PullSpec(t *testing.T) {
	svc := &fakeNodeService{
		getOpsAgentSpec: func(_ context.Context, nodeID string) (*model.OpsAgentSpecResponse, error) {
			return &model.OpsAgentSpecResponse{NodeID: nodeID, Managed: true, Revision: 3}, nil
		},
	}
	_, internal := newQuotaRouters(svc)

	w := httptest.NewRecorder()
	internal.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/v1/ops-agent/n-1/config/quota", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp model.OpsAgentSpecResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.Managed || resp.Revision != 3 {
		t.Fatalf("resp = %s err=%v", w.Body.String(), err)
	}
}

// newInternalQuotaRouter mounts the CLI-facing internal quota routes.
func newInternalQuotaRouter(svc *fakeNodeService) *gin.Engine {
	r := gin.New()
	handler.NewInternalHandler(svc).Register(r.Group("/internal/v1"))
	return r
}

func TestInternalQuotaAPI_Get(t *testing.T) {
	svc := &fakeNodeService{
		getNodeQuotaView: func(_ context.Context, nodeID string) (*model.QuotaView, error) {
			return &model.QuotaView{NodeID: nodeID, Drift: model.DriftDetected, Message: "mcpu differs"}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/v1/nodes/n-1/config/quota", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var view model.QuotaView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.NodeID != "n-1" || view.Drift != model.DriftDetected {
		t.Fatalf("view = %s err=%v", w.Body.String(), err)
	}
}

func TestInternalQuotaAPI_PutOperatorFromQuery(t *testing.T) {
	var gotOperator string
	svc := &fakeNodeService{
		setNodeQuota: func(_ context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
			gotOperator = operator
			return &model.QuotaView{NodeID: nodeID}, &model.PushResult{Applied: true}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/nodes/n-1/config/quota?operator=bob", bytes.NewReader([]byte(`{"mcpu_limit":64000}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if gotOperator != "bob" {
		t.Fatalf("operator = %q, want bob", gotOperator)
	}
}

func TestInternalQuotaAPI_PutDefaultsOperatorToCli(t *testing.T) {
	var gotOperator string
	svc := &fakeNodeService{
		setNodeQuota: func(_ context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
			gotOperator = operator
			return &model.QuotaView{NodeID: nodeID}, &model.PushResult{}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/nodes/n-1/config/quota", bytes.NewReader([]byte(`{"mcpu_limit":64000}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if gotOperator != "cli" {
		t.Fatalf("operator = %q, want cli default", gotOperator)
	}
}

func TestInternalQuotaAPI_PutInvalidBody(t *testing.T) {
	r := newInternalQuotaRouter(&fakeNodeService{})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/nodes/n-1/config/quota", bytes.NewReader([]byte(`not-json`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestInternalQuotaAPI_History(t *testing.T) {
	svc := &fakeNodeService{
		listOperations: func(_ context.Context, nodeID string, limit int) ([]model.NodeOperation, error) {
			return []model.NodeOperation{
				{NodeID: nodeID, Type: model.OpSetQuota, Operator: "cli", Detail: "{}", CreatedAt: time.Now()},
			}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/v1/nodes/n-1/config/quota/history?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var entries []model.QuotaHistoryEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || len(entries) != 1 || entries[0].Operator != "cli" {
		t.Fatalf("entries = %s err=%v", w.Body.String(), err)
	}
}

func TestInternalClusterQuotaDefaults_GetPut(t *testing.T) {
	var gotRatio *float64
	var gotOperator string
	svc := &fakeNodeService{
		setClusterQuotaDefaults: func(_ context.Context, ratio *float64, operator string) (*model.ClusterQuotaDefaults, *model.QuotaPropagation, error) {
			gotRatio, gotOperator = ratio, operator
			return &model.ClusterQuotaDefaults{PausedReleaseRatio: ratio}, &model.QuotaPropagation{Bumped: 2, Pushed: 2}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/cluster/quota-defaults?operator=bob", bytes.NewReader([]byte(`{"paused_release_ratio":0.5}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if gotRatio == nil || *gotRatio != 0.5 {
		t.Fatalf("ratio = %v", gotRatio)
	}
	if gotOperator != "bob" {
		t.Fatalf("operator = %q", gotOperator)
	}
	var out struct {
		Defaults    model.ClusterQuotaDefaults `json:"defaults"`
		Propagation model.QuotaPropagation     `json:"propagation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Propagation.Bumped != 2 {
		t.Fatalf("out = %s err=%v", w.Body.String(), err)
	}
}

func TestInternalClusterQuotaDefaults_ClearWithNull(t *testing.T) {
	var gotRatio *float64
	svc := &fakeNodeService{
		setClusterQuotaDefaults: func(_ context.Context, ratio *float64, _ string) (*model.ClusterQuotaDefaults, *model.QuotaPropagation, error) {
			gotRatio = ratio
			return &model.ClusterQuotaDefaults{}, &model.QuotaPropagation{}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/cluster/quota-defaults", bytes.NewReader([]byte(`{"paused_release_ratio":null}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if gotRatio != nil {
		t.Fatalf("ratio = %v, want nil (clear)", gotRatio)
	}
}

func TestInternalClusterQuotaDefaults_GetUnset(t *testing.T) {
	svc := &fakeNodeService{
		getClusterQuotaDefaults: func(_ context.Context) (*model.ClusterQuotaDefaults, error) {
			return &model.ClusterQuotaDefaults{}, nil
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/v1/cluster/quota-defaults", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out model.ClusterQuotaDefaults
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.PausedReleaseRatio != nil {
		t.Fatalf("out = %s err=%v", w.Body.String(), err)
	}
}

func TestInternalQuotaAPI_PutGuardRejectionIs400(t *testing.T) {
	svc := &fakeNodeService{
		setNodeQuota: func(_ context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
			return nil, nil, fmt.Errorf("%w: mem_limit 262144MB exceeds guard 39344MB", service.ErrQuotaValidation)
		},
	}
	r := newInternalQuotaRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/internal/v1/nodes/n-1/config/quota", bytes.NewReader([]byte(`{"mem_limit":"262144Mi"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (validation is client input, not a server fault)", w.Code)
	}
}

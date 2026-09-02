// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/warehouse"
)

func testWH() *WarehouseHandler {
	return NewWarehouseHandler(nil, warehouse.NewMemBlobStore(), nil, nil, 0, 0)
}

func TestWarehouseInternalBlob_BadArch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	g := r.Group("/internal/warehouse")
	h.RegisterInternal(g)

	w := httptestRecorder(t, r, "GET", "/internal/warehouse/blob?arch=ppc64&component=cube-shim&version=v0.6.0")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseInternalRequiresNodeID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	g := r.Group("/internal/warehouse")
	h.RegisterInternal(g)
	w := httptestRecorder(t, r, "GET", "/internal/warehouse/jobs")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
	w = httptestRecorder(t, r, "PUT", "/internal/warehouse/inventory", `{"arch":"amd64","items":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("inventory without node: status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseInternalInventory_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	h.RegisterInternal(r.Group("/internal/warehouse"))
	req := httptest.NewRequest("PUT", "/internal/warehouse/inventory", strings.NewReader(`{`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cube-Node-ID", "node-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseInternalInventory_BadComponent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	h.RegisterInternal(r.Group("/internal/warehouse"))
	req := httptest.NewRequest("PUT", "/internal/warehouse/inventory", strings.NewReader(
		`{"arch":"amd64","items":[{"component":"cubelet","version":"v1"}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cube-Node-ID", "node-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseAdminRequiresJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jm := auth.NewJWTManager("test-secret-32-bytes-long-enough!", time.Minute, time.Hour)
	h := testWH()
	r := gin.New()
	authed := r.Group("/api/v1", auth.Middleware(jm))
	h.RegisterAdmin(authed)

	w := httptestRecorder(t, r, "GET", "/api/v1/warehouse/components")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseGetComponent_UnknownName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	h.RegisterAdmin(r.Group("/api/v1"))
	w := httptestRecorder(t, r, "GET", "/api/v1/warehouse/components/cubelet")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestWarehouseDisabledReturns501(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewWarehouseHandler(nil, nil, nil, nil, 0, 0)
	r := gin.New()
	h.RegisterAdmin(r.Group("/api/v1"))
	h.RegisterInternal(r.Group("/internal/warehouse"))

	w := httptestRecorder(t, r, "GET", "/api/v1/warehouse/components")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("admin status=%d want 501 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), warehouse.CodeDisabled) {
		t.Fatalf("body=%s want warehouse_disabled", w.Body.String())
	}
	req := httptest.NewRequest("GET", "/internal/warehouse/blob?arch=amd64&component=cube-shim&version=v1", nil)
	req.Header.Set("X-Cube-Node-ID", "node-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("blob status=%d want 501 body=%s", rec.Code, rec.Body.String())
	}
}

func TestWarehouseGetBlobRequiresNodeID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := testWH()
	r := gin.New()
	h.RegisterInternal(r.Group("/internal/warehouse"))
	w := httptestRecorder(t, r, "GET", "/internal/warehouse/blob?arch=amd64&component=cube-shim&version=v1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestKnownComponentCatalog(t *testing.T) {
	if !warehouse.KnownComponent("cube-shim") {
		t.Fatal("shim should be known")
	}
	if warehouse.KnownComponent("not-a-component") {
		t.Fatal("unknown should be rejected")
	}
}

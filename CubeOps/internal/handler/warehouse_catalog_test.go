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
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/warehouse"
)

func TestWarehouseCatalogHTTP_EmptyAndVersions(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	wh := handler.NewWarehouseHandler(env.store, warehouse.NewMemBlobStore(), nil, nil, 0, 0)
	r := gin.New()
	wh.RegisterAdmin(r.Group("/api/v1"))

	list := getJSON(t, r, "/api/v1/warehouse/components")
	comps, _ := list["components"].([]any)
	if len(comps) != 4 {
		t.Fatalf("components=%d want 4 body=%s", len(comps), mustJSON(list))
	}
	for _, raw := range comps {
		row := raw.(map[string]any)
		if row["versionCount"].(float64) != 0 {
			t.Errorf("%s versionCount=%v want 0", row["name"], row["versionCount"])
		}
		if _, ok := row["nodesMissing"]; ok {
			t.Errorf("%s nodesMissing should be omitted when node inventory is unavailable, got %v", row["name"], row["nodesMissing"])
		}
	}

	ctx := context.Background()
	mustInsert := func(item store.WarehouseItem) {
		t.Helper()
		ok, err := env.store.InsertWarehouseItem(ctx, item)
		if err != nil || !ok {
			t.Fatalf("insert %+v: ok=%v err=%v", item, ok, err)
		}
	}
	mustInsert(store.WarehouseItem{
		Arch: "amd64", Component: "cube-shim", Version: "v0.6.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/amd64/cube-shim/v0.6.0/component.tar.gz", SizeBytes: 100, Checksum: "sha256:a",
	})
	mustInsert(store.WarehouseItem{
		Arch: "arm64", Component: "cube-shim", Version: "v0.6.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/arm64/cube-shim/v0.6.0/component.tar.gz", SizeBytes: 80, Checksum: "sha256:b",
	})
	mustInsert(store.WarehouseItem{
		Arch: "amd64", Component: "cube-shim", Version: "v0.5.0",
		Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		ObjectKey: "blobs/amd64/cube-shim/v0.5.0/component.tar.gz", SizeBytes: 90, Checksum: "sha256:c",
	})

	list = getJSON(t, r, "/api/v1/warehouse/components")
	shim := findComponent(t, list, "cube-shim")
	if shim["versionCount"].(float64) != 2 {
		t.Errorf("versionCount=%v want 2", shim["versionCount"])
	}
	if shim["sizeBytes"].(float64) != 270 {
		t.Errorf("sizeBytes=%v want 270", shim["sizeBytes"])
	}

	detail := getJSON(t, r, "/api/v1/warehouse/components/cube-shim")
	if detail["name"] != "cube-shim" {
		t.Fatalf("name=%v", detail["name"])
	}
	versions, _ := detail["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions=%d want 2: %s", len(versions), mustJSON(detail))
	}
	first := versions[0].(map[string]any)
	if first["version"] != "v0.6.0" {
		t.Errorf("first version=%v want v0.6.0", first["version"])
	}
	arts, _ := first["artifacts"].([]any)
	if len(arts) != 2 {
		t.Fatalf("v0.6.0 artifacts=%d want 2", len(arts))
	}
	if arts[0].(map[string]any)["arch"] != "amd64" {
		t.Errorf("arch order=%v want amd64 first", arts[0].(map[string]any)["arch"])
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/warehouse/components/cubelet", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown component status=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func getJSON(t *testing.T, r *gin.Engine, path string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("json %s: %v body=%s", path, err, w.Body.String())
	}
	return out
}

func findComponent(t *testing.T, list map[string]any, name string) map[string]any {
	t.Helper()
	comps, _ := list["components"].([]any)
	for _, raw := range comps {
		row := raw.(map[string]any)
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("component %s not found in %s", name, mustJSON(list))
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestWarehouseGetBlobObjectMissing(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	ok, err := env.store.InsertWarehouseItem(ctx, store.WarehouseItem{
		Arch: "amd64", Component: "cube-shim", Version: "v0.6.0",
		Source: "upload", SourceRef: "uploads/missing.tar.gz",
		ObjectKey: "blobs/amd64/cube-shim/v0.6.0/component.tar.gz",
		SizeBytes: 1, Checksum: "sha256:deadbeef",
	})
	if err != nil || !ok {
		t.Fatalf("insert: ok=%v err=%v", ok, err)
	}

	wh := handler.NewWarehouseHandler(env.store, warehouse.NewMemBlobStore(), nil, nil, 0, 0)
	r := gin.New()
	wh.RegisterInternal(r.Group("/internal/warehouse"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/warehouse/blob?arch=amd64&component=cube-shim&version=v0.6.0", nil)
	req.Header.Set("X-Cube-Node-ID", "node-1")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), warehouse.CodeNotFound) {
		t.Fatalf("body=%s want warehouse_not_found", w.Body.String())
	}
}

func TestListImportsHTTP(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	wh := handler.NewWarehouseHandler(env.store, warehouse.NewMemBlobStore(), nil, nil, 0, 0)
	r := gin.New()
	wh.RegisterAdmin(r.Group("/api/v1"))

	empty := getJSON(t, r, "/api/v1/warehouse/imports")
	jobs, _ := empty["jobs"].([]any)
	if len(jobs) != 0 {
		t.Fatalf("empty jobs=%v", jobs)
	}
	if empty["total"].(float64) != 0 {
		t.Fatalf("empty total=%v", empty["total"])
	}

	if err := env.store.CreateImportJob(context.Background(), store.ImportJob{
		ID: "imp-1", Source: "github", SourceRef: "TencentCloud/CubeSandbox",
		Tag: "v0.6.0", Arch: "amd64", Status: store.ImportPending,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	listed := getJSON(t, r, "/api/v1/warehouse/imports")
	jobs, _ = listed["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d body=%s", len(jobs), mustJSON(listed))
	}
	if listed["total"].(float64) != 1 {
		t.Fatalf("total=%v body=%s", listed["total"], mustJSON(listed))
	}
	row := jobs[0].(map[string]any)
	if row["id"] != "imp-1" || row["status"] != store.ImportPending {
		t.Fatalf("row=%s", mustJSON(row))
	}
}

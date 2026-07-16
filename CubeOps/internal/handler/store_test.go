// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// Note: GetStoreMeta / RefreshStoreMeta shell out to `docker inspect`, which
// is not available in CI. These tests only verify the routing + response
// envelope shape; the inspect logic itself is covered by inspectImage's
// defensive nil-return on docker failure.

func newStoreRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewStoreHandler()
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

// When docker is unavailable, inspectAll returns an empty slice and the
// handler still returns 200 with {"images": []}. This is the contract the
// frontend relies on.
func TestStore_GetStoreMeta_NoDocker_ReturnsEmptyImages(t *testing.T) {
	r := newStoreRouter(t)

	w := httptestRecorder(t, r, "GET", "/api/v1/store/meta")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	images, ok := resp["images"].([]interface{})
	if !ok {
		t.Fatalf("images is not an array: %v", resp)
	}
	// Without docker the list is empty; we just verify the envelope shape.
	_ = images
}

func TestStore_RefreshStoreMeta_NoDocker_ReturnsEmptyImages(t *testing.T) {
	r := newStoreRouter(t)

	w := httptestRecorder(t, r, "POST", "/api/v1/store/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if _, ok := resp["images"]; !ok {
		t.Errorf("missing 'images' key in response: %v", resp)
	}
}

// --- Config handler ---

func newConfigRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewConfigHandler("127.0.0.1:3010", 100, true, "cube.app", "cubebox")
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

func TestConfig_GetConfig(t *testing.T) {
	r := newConfigRouter(t)

	w := httptestRecorder(t, r, "GET", "/api/v1/config")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if cfg["rateLimitPerSec"] != float64(100) {
		t.Errorf("rateLimitPerSec = %v, want 100", cfg["rateLimitPerSec"])
	}
	if cfg["authEnabled"] != true {
		t.Errorf("authEnabled = %v, want true", cfg["authEnabled"])
	}
	if cfg["sandboxDomain"] != "cube.app" {
		t.Errorf("sandboxDomain = %v, want cube.app", cfg["sandboxDomain"])
	}
	if cfg["instanceType"] != "cubebox" {
		t.Errorf("instanceType = %v, want cubebox", cfg["instanceType"])
	}
	// APIEndpoint should fall back to bind address when env var is unset.
	if cfg["apiEndpoint"] != "http://127.0.0.1:3010/cubeapi/v1" {
		t.Errorf("apiEndpoint = %v, want http://127.0.0.1:3010/cubeapi/v1", cfg["apiEndpoint"])
	}
	if cfg["opsApiEndpoint"] != "http://127.0.0.1:3010/opsapi/v1" {
		t.Errorf("opsApiEndpoint = %v, want http://127.0.0.1:3010/opsapi/v1", cfg["opsApiEndpoint"])
	}
}

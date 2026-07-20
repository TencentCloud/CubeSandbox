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

// Note: GetStoreMeta / RefreshStoreMeta use go-containerregistry to talk
// to real registries. Registry access is abstracted behind RegistryClient,
// which makes the handler fully unit-testable without network access. The
// fake client below returns deterministic metadata for the configured
// store images.

type fakeRegistryClient struct {
	meta map[string]ImageMeta
}

func (f *fakeRegistryClient) FetchLatest(_ context.Context, ref string) *ImageMeta {
	if m, ok := f.meta[ref]; ok {
		return &m
	}
	return nil
}

func (f *fakeRegistryClient) Cached(ref string) *ImageMeta {
	return f.FetchLatest(nil, ref)
}

func newStoreRouterWithFake(t *testing.T) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := &StoreHandler{
		registryClient: &fakeRegistryClient{
			meta: map[string]ImageMeta{
				"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest": {
					Image:       "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
					SizeBytes:   1024 * 1024 * 100,
					SizeMB:      100,
					Digest:      strPtr("cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code@sha256:abc123"),
					DigestShort: strPtr("sha256:abc123"),
				},
				"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest": {
					Image:       "cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
					SizeBytes:   1024 * 1024 * 200,
					SizeMB:      200,
					Digest:      strPtr("cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser@sha256:def456"),
					DigestShort: strPtr("sha256:def456"),
				},
				"ghcr.io/tencentcloud/cubesandbox-base:latest": {
					Image:       "ghcr.io/tencentcloud/cubesandbox-base:latest",
					SizeBytes:   1024 * 1024 * 300,
					SizeMB:      300,
					Digest:      strPtr("ghcr.io/tencentcloud/cubesandbox-base@sha256:789012"),
					DigestShort: strPtr("sha256:789012"),
				},
			},
		},
	}
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

func strPtr(s string) *string { return &s }

func TestStore_GetStoreMeta_ReturnsAllImages(t *testing.T) {
	r := newStoreRouterWithFake(t)

	w := httptestRecorder(t, r, "GET", "/api/v1/store/meta", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp StoreMeta
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Images) != len(storeImages) {
		t.Fatalf("images count = %d, want %d", len(resp.Images), len(storeImages))
	}
	// Each entry must carry a digest when the fake client has it.
	for _, img := range resp.Images {
		if img.Digest == nil {
			t.Errorf("image %s missing digest", img.Image)
		}
	}
}

func TestStore_RefreshStoreMeta_ReturnsAllImages(t *testing.T) {
	r := newStoreRouterWithFake(t)

	w := httptestRecorder(t, r, "POST", "/api/v1/store/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp StoreMeta
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Images) != len(storeImages) {
		t.Fatalf("images count = %d, want %d", len(resp.Images), len(storeImages))
	}
}

// When the registry client cannot resolve an image, the handler must
// still return a placeholder entry (image only, nil digest) so the
// frontend can render the store entry and later retry.
func TestStore_RefreshStoreMeta_MissingImage_StillReturnsPlaceholder(t *testing.T) {
	r := gin.New()
	h := &StoreHandler{
		registryClient: &fakeRegistryClient{meta: map[string]ImageMeta{}},
	}
	g := r.Group("/api/v1")
	h.Register(g)

	w := httptestRecorder(t, r, "POST", "/api/v1/store/refresh", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp StoreMeta
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Images) != len(storeImages) {
		t.Fatalf("images count = %d, want %d (placeholders expected)", len(resp.Images), len(storeImages))
	}
	for _, img := range resp.Images {
		if img.Digest != nil {
			t.Errorf("expected nil digest for unresolved image %s, got %v", img.Image, *img.Digest)
		}
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Note: GetStoreMeta / RefreshStoreMeta use go-containerregistry to talk
// to real registries. Registry access is abstracted behind RegistryClient,
// which makes the handler fully unit-testable without network access.
//
// Two fakes are provided to exercise the two halves of the contract
// independently:
//
//   - cachingFakeFetchOnly: FetchLatest populates an internal cache; Cached
//     reads it. Models the production gcrRegistryClient.
//   - preloadedFake: Cached answers from a pre-populated map; FetchLatest
//     always fails. Models a state where /store/refresh has never been
//     called yet but we still want to verify GetStoreMeta's placeholder
//     behaviour is consistent.

type cachingFakeFetchOnly struct {
	mu   sync.Mutex
	meta map[string]ImageMeta
}

func (f *cachingFakeFetchOnly) FetchLatest(_ context.Context, ref string) *ImageMeta {
	m, ok := f.meta[ref]
	if !ok {
		return nil
	}
	f.mu.Lock()
	f.meta[ref] = m
	f.mu.Unlock()
	return &m
}

func (f *cachingFakeFetchOnly) Cached(ref string) *ImageMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[ref]
	if !ok {
		return nil
	}
	return &m
}

type preloadedFake struct {
	meta map[string]ImageMeta
}

func (f *preloadedFake) FetchLatest(_ context.Context, _ string) *ImageMeta {
	return nil
}

func (f *preloadedFake) Cached(ref string) *ImageMeta {
	if m, ok := f.meta[ref]; ok {
		return &m
	}
	return nil
}

func fakeStoreMeta() map[string]ImageMeta {
	return map[string]ImageMeta{
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
	}
}

func newCachingFake() *cachingFakeFetchOnly {
	return &cachingFakeFetchOnly{meta: fakeStoreMeta()}
}

func newPreloadedFake() *preloadedFake {
	return &preloadedFake{meta: fakeStoreMeta()}
}

// jitterFetchFake delays each FetchLatest call by a per-image duration
// so the goroutine completion order is intentionally different from
// the launch order. The handler must still emit results in storeImages
// order regardless of which goroutine wins the race.
type jitterFetchFake struct {
	images []string
}

func (f *jitterFetchFake) FetchLatest(ctx context.Context, ref string) *ImageMeta {
	// Reverse the natural order so the last-launched goroutine wakes
	// up first; reverse the middle and last positions too.
	pos := -1
	for i, img := range f.images {
		if img == ref {
			pos = i
			break
		}
	}
	if pos < 0 {
		return nil
	}
	// Sleep = (len - pos) * 5ms. Position 0 waits longest, position
	// N-1 returns immediately. Completion order is therefore the
	// reverse of storeImages.
	delay := time.Duration(len(f.images)-pos) * 5 * time.Millisecond
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil
	}
	return &ImageMeta{
		Image:       ref,
		SizeBytes:   1,
		SizeMB:      0,
		Digest:      strPtr(ref + "@sha256:fake"),
		DigestShort: strPtr("sha256:fake"),
	}
}

func (f *jitterFetchFake) Cached(_ string) *ImageMeta { return nil }

func strPtr(s string) *string { return &s }

func newStoreRouterWithFake(t *testing.T) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewStoreHandler(newCachingFake())
	g := r.Group("/api/v1")
	h.Register(g)
	return r
}

// resetStoreRefreshLimiter clears the package-level limiter so each
// test starts with a fresh window. Required because the limiter is a
// process-wide singleton.
func resetStoreRefreshLimiter(t *testing.T) {
	t.Helper()
	defaultStoreRefreshLimiter.mu.Lock()
	defaultStoreRefreshLimiter.last = time.Time{}
	defaultStoreRefreshLimiter.mu.Unlock()
}

func TestStore_GetStoreMeta_ReturnsAllImages(t *testing.T) {
	resetStoreRefreshLimiter(t)
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
	for _, img := range resp.Images {
		if img.Digest == nil {
			t.Errorf("image %s missing digest", img.Image)
		}
	}
}

func TestStore_RefreshStoreMeta_ReturnsAllImages(t *testing.T) {
	resetStoreRefreshLimiter(t)
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
	// Same invariant as the GET path: every refreshed image must carry
	// a digest.
	for _, img := range resp.Images {
		if img.Digest == nil {
			t.Errorf("image %s missing digest", img.Image)
		}
	}
}

// TestStore_GetStoreMeta_AfterRefresh_UsesCache verifies the caching
// contract: after a successful /store/refresh, GET /store/meta returns
// the cached data without calling FetchLatest again. We use the
// preloadedFake (FetchLatest always returns nil) to assert that the
// GET path does not fall through to FetchLatest.
func TestStore_GetStoreMeta_AfterRefresh_UsesCache(t *testing.T) {
	resetStoreRefreshLimiter(t)
	r := gin.New()
	h := NewStoreHandler(newPreloadedFake())
	g := r.Group("/api/v1")
	h.Register(g)

	// No /store/refresh has been called yet — preloadedFake has all
	// three images cached. GET should serve them straight from the
	// cache.
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
	for _, img := range resp.Images {
		if img.Digest == nil {
			t.Errorf("image %s missing digest (cache should be served)", img.Image)
		}
	}
}

// TestStore_Refresh_OutputOrderIsDeterministic verifies that the order
// of the returned images matches storeImages regardless of which
// goroutine finishes first. The fake delays each fetch by a different
// amount so completion order is intentionally scrambled.
func TestStore_Refresh_OutputOrderIsDeterministic(t *testing.T) {
	resetStoreRefreshLimiter(t)
	r := gin.New()
	h := NewStoreHandler(&jitterFetchFake{images: storeImages})
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
		t.Fatalf("images count = %d, want %d", len(resp.Images), len(storeImages))
	}
	for i, want := range storeImages {
		if resp.Images[i].Image != want {
			t.Errorf("resp.Images[%d] = %q, want %q (order should match storeImages)", i, resp.Images[i].Image, want)
		}
	}
}

// TestStore_GetStoreMeta_NoCache_ReturnsPlaceholders verifies the
// fallback contract: with no cache and a FetchLatest that always
// fails, GET still returns a placeholder per image.
func TestStore_GetStoreMeta_NoCache_ReturnsPlaceholders(t *testing.T) {
	resetStoreRefreshLimiter(t)
	r := gin.New()
	h := NewStoreHandler(&preloadedFake{meta: map[string]ImageMeta{}})
	g := r.Group("/api/v1")
	h.Register(g)

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
	for _, img := range resp.Images {
		if img.Digest != nil {
			t.Errorf("expected nil digest for empty cache, got %v", *img.Digest)
		}
	}
}

// When the registry client cannot resolve an image, the handler must
// still return a placeholder entry (image only, nil digest) so the
// frontend can render the store entry and later retry.
func TestStore_RefreshStoreMeta_MissingImage_StillReturnsPlaceholder(t *testing.T) {
	resetStoreRefreshLimiter(t)
	r := gin.New()
	h := NewStoreHandler(&cachingFakeFetchOnly{meta: map[string]ImageMeta{}})
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

// TestStore_DefaultClient_HandlesUnparseableRef verifies that the
// production DefaultRegistryClient surfaces a placeholder entry when
// given an unparseable image reference, without panicking.
func TestStore_DefaultClient_HandlesUnparseableRef(t *testing.T) {
	resetStoreRefreshLimiter(t)
	orig := storeImages
	t.Cleanup(func() { storeImages = orig })

	// A reference with an unparseable format (raw CR characters) makes
	// name.ParseReference return an error before any network call, so
	// the test stays hermetic and fast.
	storeImages = []string{"not a valid ref !!!"}

	r := gin.New()
	h := NewStoreHandler(DefaultRegistryClient())
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
	if len(resp.Images) != 1 {
		t.Fatalf("images count = %d, want 1 (placeholder expected)", len(resp.Images))
	}
	if resp.Images[0].Digest != nil {
		t.Errorf("expected nil digest for unresolvable ref, got %v", *resp.Images[0].Digest)
	}
	if resp.Images[0].Image != storeImages[0] {
		t.Errorf("image = %q, want %q", resp.Images[0].Image, storeImages[0])
	}
}

// TestStore_RefreshRateLimit verifies that POST /store/refresh is
// rate-limited: a second call within the 10s minimum interval is
// rejected with 429.
func TestStore_RefreshRateLimit(t *testing.T) {
	resetStoreRefreshLimiter(t)
	r := newStoreRouterWithFake(t)

	// First call must succeed.
	w1 := httptestRecorder(t, r, "POST", "/api/v1/store/refresh", "")
	if w1.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}

	// Second call within the window must be rejected.
	w2 := httptestRecorder(t, r, "POST", "/api/v1/store/refresh", "")
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429; body=%s", w2.Code, w2.Body.String())
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

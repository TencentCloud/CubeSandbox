// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubeproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestNormalizeAdminURLs(t *testing.T) {
	got := normalizeAdminURLs([]string{
		" http://10.0.0.1:8082/ ",
		"10.0.0.2:8082",
		"http://10.0.0.1:8082",
		"",
	})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got[0] != "http://10.0.0.1:8082" || got[1] != "http://10.0.0.2:8082" {
		t.Fatalf("got %v", got)
	}
}

func TestInvalidateBackendCacheBroadcast(t *testing.T) {
	origList := listAdminURLsFn
	origDo := doDeleteFn
	defer func() {
		listAdminURLsFn = origList
		doDeleteFn = origDo
	}()

	listAdminURLsFn = func(context.Context, string) []string {
		return []string{"http://a:8082", "http://b:8082"}
	}
	var hits int32
	doDeleteFn = func(_ context.Context, adminURL, sandboxID string) error {
		if sandboxID != "sb-1" {
			t.Fatalf("sandboxID=%q", sandboxID)
		}
		atomic.AddInt32(&hits, 1)
		_ = adminURL
		return nil
	}

	InvalidateBackendCache(context.Background(), "sb-1", "9.9.9.9")
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestPostBackendCacheDeleteHTTP(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != backendCacheDeletePath {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"deleted":3}`))
	}))
	defer srv.Close()

	if err := postBackendCacheDelete(context.Background(), srv.URL, "sb-9"); err != nil {
		t.Fatal(err)
	}
	if gotBody["sandbox_id"] != "sb-9" {
		t.Fatalf("body=%v", gotBody)
	}
}

func TestListAdminURLsStaticConfig(t *testing.T) {
	cfg, err := config.Init()
	if err != nil {
		t.Skipf("config.Init: %v", err)
	}
	if cfg.CubeProxyConf == nil {
		cfg.CubeProxyConf = &config.CubeProxyConf{}
	}
	prev := cfg.CubeProxyConf.AdminURLs
	cfg.CubeProxyConf.AdminURLs = []string{"http://10.1.1.1:8082", "10.1.1.2:8082"}
	defer func() { cfg.CubeProxyConf.AdminURLs = prev }()

	got := listAdminURLs(context.Background(), "ignored")
	if len(got) != 2 || got[0] != "http://10.1.1.1:8082" || got[1] != "http://10.1.1.2:8082" {
		t.Fatalf("got %v", got)
	}
}

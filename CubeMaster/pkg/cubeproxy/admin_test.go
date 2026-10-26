// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubeproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

	if err := InvalidateBackendCache(context.Background(), "sb-1", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits=%d", hits)
	}
}

// A replica we never convinced keeps routing to the pre-resume backend, so
// the caller has to hear about it even when its peers accepted the purge.
func TestInvalidateBackendCachePartialIsAnError(t *testing.T) {
	origList := listAdminURLsFn
	origDo := doDeleteFn
	defer func() {
		listAdminURLsFn = origList
		doDeleteFn = origDo
	}()

	listAdminURLsFn = func(context.Context, string) []string {
		return []string{"http://ok:8082", "http://stale:8082"}
	}
	var staleHits int32
	doDeleteFn = func(_ context.Context, adminURL, _ string) error {
		if adminURL == "http://stale:8082" {
			atomic.AddInt32(&staleHits, 1)
			return errors.New("no route")
		}
		return nil
	}

	err := InvalidateBackendCache(context.Background(), "sb-1", "9.9.9.9")
	if err == nil {
		t.Fatal("partial purge reported success")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error does not name the replica: %v", err)
	}
	// Retried, and only the replica that refused.
	if got := atomic.LoadInt32(&staleHits); got != purgeAttempts {
		t.Fatalf("stale replica attempts=%d want %d", got, purgeAttempts)
	}
}

func TestInvalidateBackendCacheRetrySucceeds(t *testing.T) {
	origList := listAdminURLsFn
	origDo := doDeleteFn
	defer func() {
		listAdminURLsFn = origList
		doDeleteFn = origDo
	}()

	listAdminURLsFn = func(context.Context, string) []string { return []string{"http://a:8082"} }
	var hits int32
	doDeleteFn = func(context.Context, string, string) error {
		if atomic.AddInt32(&hits, 1) == 1 {
			return errors.New("connection refused")
		}
		return nil
	}

	if err := InvalidateBackendCache(context.Background(), "sb-1", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidateBackendCacheNoEndpoint(t *testing.T) {
	origList := listAdminURLsFn
	defer func() { listAdminURLsFn = origList }()

	listAdminURLsFn = func(context.Context, string) []string { return nil }
	if err := InvalidateBackendCache(context.Background(), "sb-1", ""); err == nil {
		t.Fatal("missing admin endpoint reported success")
	}
}

// 404 means the replica answered but predates the admin route — a deployment
// problem the message has to name, since it is indistinguishable from a
// healthy proxy until a resumed sandbox turns out unreachable.
func TestPostBackendCacheDeleteNotFoundNamesOldProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	err := postBackendCacheDelete(context.Background(), srv.URL, "sb-9")
	if err == nil {
		t.Fatal("404 reported success")
	}
	if !strings.Contains(err.Error(), "too old") {
		t.Fatalf("error does not explain the 404: %v", err)
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

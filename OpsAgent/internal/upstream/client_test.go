// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPullSpecDecodes(t *testing.T) {
	body := `{"node_id":"n1","managed":true,"revision":3,` +
		`"spec":{"mcpu_limit":64000,"mem_limit":"64Gi","paused_resource_release_ratio":0.5},` +
		`"physical":{"cpu_total":16,"mem_mb_total":39799},"only_paused_ratio":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/ops-agent/n1/config/quota" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	resp, err := New(srv.URL).PullSpec(context.Background(), "n1")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !resp.Managed || resp.Revision != 3 || resp.Spec == nil || resp.Spec.MCpuLimit != 64000 {
		t.Fatalf("spec: %+v", resp.Spec)
	}
	// Physical must decode (json tags, not the struct field names).
	if resp.Physical.CpuTotal != 16 || resp.Physical.MemMBTotal != 39799 {
		t.Fatalf("physical: %+v", resp.Physical)
	}
}

func TestPullSpecNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := New(srv.URL).PullSpec(context.Background(), "n1")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want not-registered error, got %v", err)
	}
}

func TestPullSpecNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(srv.URL).PullSpec(context.Background(), "n1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status error, got %v", err)
	}
}

func TestPullSpecBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	_, err := New(srv.URL).PullSpec(context.Background(), "n1")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

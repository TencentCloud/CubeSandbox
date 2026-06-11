// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeegress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientNotConfigured(t *testing.T) {
	c := New("", 0)
	if c.Configured() {
		t.Fatal("Configured()=true for empty URL")
	}
	err := c.PutPolicy(context.Background(), "192.168.0.10", nil)
	if err != ErrNotConfigured {
		t.Fatalf("PutPolicy err=%v, want ErrNotConfigured", err)
	}
	err = c.DeletePolicy(context.Background(), "192.168.0.10")
	if err != ErrNotConfigured {
		t.Fatalf("DeletePolicy err=%v, want ErrNotConfigured", err)
	}
}

func TestClientPutPolicySuccess(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotCT     string
		gotBody   map[string]any
		callCount int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "deepseek_api",
			Match:  &MatchInput{Host: strPtr("api.deepseek.com")},
			Action: &ActionInput{Allow: true},
		}},
	}
	if err := c.PutPolicy(context.Background(), "192.168.0.10", in); err != nil {
		t.Fatalf("PutPolicy err=%v", err)
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("callCount=%d, want 1", callCount)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method=%s, want PUT", gotMethod)
	}
	if gotPath != "/admin/v1/policies/192.168.0.10" {
		t.Fatalf("path=%s", gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type=%q", gotCT)
	}
	if gotBody["policy_id"] != "192.168.0.10" {
		t.Fatalf("policy_id=%v", gotBody["policy_id"])
	}
}

func TestClientPutPolicySkipsWhenNoRules(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	if err := c.PutPolicy(context.Background(), "192.168.0.10", &PolicyInput{}); err != nil {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Fatal("HTTP server was called for empty input; should have been skipped")
	}
}

func TestClientPutPolicy4xxReturnsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"rules[0].action.inject[0].secret required"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "r1",
			Match:  &MatchInput{Host: strPtr("x.com")},
			Action: &ActionInput{Allow: true},
		}},
	}
	err := c.PutPolicy(context.Background(), "192.168.0.10", in)
	if err == nil {
		t.Fatal("err=nil, want permanent error")
	}
	if !IsPermanent(err) {
		t.Fatalf("err=%v, want IsPermanent=true (the maintenance loop must NOT retry 4xx)", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("err=%v should mention status 400", err)
	}
}

func TestClientPutPolicy5xxReturnsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "r1",
			Match:  &MatchInput{Host: strPtr("x.com")},
			Action: &ActionInput{Allow: true},
		}},
	}
	err := c.PutPolicy(context.Background(), "192.168.0.10", in)
	if err == nil {
		t.Fatal("err=nil, want transient error")
	}
	// 5xx is NOT permanent — the retry loop must be willing to retry.
	if IsPermanent(err) {
		t.Fatalf("5xx classified as permanent: %v", err)
	}
}

func TestClientDeletePolicy(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	if err := c.DeletePolicy(context.Background(), "192.168.0.10"); err != nil {
		t.Fatalf("DeletePolicy err=%v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method=%s", gotMethod)
	}
	if gotPath != "/admin/v1/policies/192.168.0.10" {
		t.Fatalf("path=%s", gotPath)
	}
}

func TestClientRejectsBadIP(t *testing.T) {
	c := New("http://127.0.0.1:9090", 2*time.Second)
	for _, bad := range []string{"", "../etc/passwd", "1.2.3.4/24", "1.2.3.4?evil"} {
		if err := c.PutPolicy(context.Background(), bad, nil); err == nil {
			t.Fatalf("PutPolicy(%q) err=nil, want validation failure", bad)
		}
		if err := c.DeletePolicy(context.Background(), bad); err == nil {
			t.Fatalf("DeletePolicy(%q) err=nil, want validation failure", bad)
		}
	}
}

func TestClientHonorsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow CubeEgress.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	// Caller-supplied 50ms deadline must win over the client's default
	// 2s timeout — the call should fail fast.
	c := New(srv.URL, 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	in := &PolicyInput{
		Rules: []RuleInput{{
			Name:   "r1",
			Match:  &MatchInput{Host: strPtr("x.com")},
			Action: &ActionInput{Allow: true},
		}},
	}
	start := time.Now()
	err := c.PutPolicy(ctx, "192.168.0.10", in)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err=nil, want context deadline exceeded")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed=%s; deadline was 50ms", elapsed)
	}
}

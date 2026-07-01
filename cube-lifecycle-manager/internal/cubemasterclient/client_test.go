// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubemasterclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKill_Success(t *testing.T) {
	var capturedMethod, capturedPath string
	var capturedBody killRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		_, _ = w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	if err := c.Kill(context.Background(), "sbx-1", "cubebox", KillReasonTimeout); err != nil {
		t.Fatalf("Kill returned err: %v", err)
	}
	if capturedMethod != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", capturedMethod)
	}
	if capturedPath != "/cube/sandbox" {
		t.Fatalf("expected path /cube/sandbox, got %s", capturedPath)
	}
	if capturedBody.SandboxID != "sbx-1" || capturedBody.InstanceType != "cubebox" {
		t.Fatalf("unexpected body: %+v", capturedBody)
	}
	if !capturedBody.Sync {
		t.Fatalf("Kill must request sync=true so the sweeper can evict on success: %+v", capturedBody)
	}
	if capturedBody.KillReason != KillReasonTimeout {
		t.Fatalf("kill_reason should be propagated, got %q", capturedBody.KillReason)
	}
}

func TestKill_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ret":{"ret_code":130483,"ret_msg":"key not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	err := c.Kill(context.Background(), "sbx-1", "cubebox", KillReasonRequest)
	if err == nil {
		t.Fatal("expected error for non-success ret_code")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if !apiErr.IsNotFound() {
		t.Fatalf("expected IsNotFound()=true, got false (%+v)", apiErr)
	}
}

func TestKill_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ret":{"ret_code":500,"ret_msg":"boom"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, time.Second)
	err := c.Kill(context.Background(), "sbx-1", "cubebox", "")
	if err == nil {
		t.Fatal("expected error on http 500")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("HTTP-level error should NOT be an *APIError, got %v", err)
	}
}

func TestKill_RequiresArgs(t *testing.T) {
	c := New("http://unused", time.Second)
	if err := c.Kill(context.Background(), "", "cubebox", ""); err == nil {
		t.Fatal("Kill must error on empty sandbox_id")
	}
	if err := c.Kill(context.Background(), "sbx", "", ""); err == nil {
		t.Fatal("Kill must error on empty instance_type")
	}
}

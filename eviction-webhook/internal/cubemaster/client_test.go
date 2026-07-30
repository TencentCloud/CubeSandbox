// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubemaster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsolateNode(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	if err := c.IsolateNode(context.Background(), "worker-01"); err != nil {
		t.Fatalf("IsolateNode: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: want PUT, got %s", gotMethod)
	}
	if gotPath != "/internal/meta/nodes/worker-01/isolation" {
		t.Errorf("path: want /internal/meta/nodes/worker-01/isolation, got %s", gotPath)
	}
}

func TestUnisolateNode(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	if err := c.UnisolateNode(context.Background(), "worker-01"); err != nil {
		t.Fatalf("UnisolateNode: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: want DELETE, got %s", gotMethod)
	}
	if gotPath != "/internal/meta/nodes/worker-01/isolation" {
		t.Errorf("path: %s", gotPath)
	}
}

func TestPauseSandbox(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	if err := c.PauseSandbox(context.Background(), "sandbox-abc", "cubebox", "req-1"); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}

	var req sandboxUpdateReq
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if req.Action != "pause" {
		t.Errorf("action: want pause, got %s", req.Action)
	}
	if req.SandboxID != "sandbox-abc" {
		t.Errorf("sandbox_id: want sandbox-abc, got %s", req.SandboxID)
	}
	if req.InstanceType != "cubebox" {
		t.Errorf("instance_type: want cubebox, got %s", req.InstanceType)
	}
	if req.RequestID != "req-1" {
		t.Errorf("requestID: want req-1, got %s", req.RequestID)
	}
}

func TestResumeSandbox(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	if err := c.ResumeSandbox(context.Background(), "sandbox-abc", "cubebox", "req-2"); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}

	var req sandboxUpdateReq
	json.Unmarshal(body, &req)
	if req.Action != "resume" {
		t.Errorf("action: want resume, got %s", req.Action)
	}
}

func TestClientErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.IsolateNode(context.Background(), "node-x")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500: %v", err)
	}
}

func TestClientSendsAuthHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"cube_version", "cube_user_id", "cube_signature"} {
			if r.Header.Get(h) == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-user", "test-secret", true)
	if err := c.IsolateNode(context.Background(), "node-auth"); err != nil {
		t.Fatalf("IsolateNode with auth: %v", err)
	}
}

func TestClientCubeMasterRetCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"sandbox not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.PauseSandbox(context.Background(), "no-such-sandbox", "cubebox", "req-x")
	if err == nil {
		t.Fatal("expected error when ret_code != 200")
	}
	if !strings.Contains(err.Error(), "100501") {
		t.Errorf("error should contain ret_code: %v", err)
	}
}

func TestIsolateNodeRetCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"node not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.IsolateNode(context.Background(), "missing-node")
	if err == nil {
		t.Fatal("expected error when isolation ret_code != 200")
	}
	if !strings.Contains(err.Error(), "100501") {
		t.Errorf("error should contain ret_code: %v", err)
	}
}

func TestIsolateNodeRejectsMalformedResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "invalid-json", body: `not-json`, want: "unmarshal"},
		{name: "missing-ret", body: `{"data":{}}`, want: "missing CubeMaster ret envelope"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(srv.URL, "", "", false)
			err := c.IsolateNode(context.Background(), "node-bad-response")
			if err == nil {
				t.Fatal("expected malformed response to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to contain %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSandboxUpdateAlreadyInStateIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":130490,"ret_msg":"already in desired state"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	if err := c.PauseSandbox(context.Background(), "sandbox-already", "cubebox", "req-already"); err != nil {
		t.Fatalf("PauseSandbox should treat ret_code 130490 as success: %v", err)
	}
	if err := c.ResumeSandbox(context.Background(), "sandbox-already", "cubebox", "req-already"); err != nil {
		t.Fatalf("ResumeSandbox should treat ret_code 130490 as success: %v", err)
	}
}

func TestListSandboxesByNode(t *testing.T) {
	var gotPath string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"ret": {"ret_code": 200, "ret_msg": "ok"},
			"data": [
				{"sandbox_id": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", "status": 2},
				{"sandbox_id": "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6", "status": 2}
			]
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	sandboxes, err := c.ListSandboxesByNode(context.Background(), "worker-01")
	if err != nil {
		t.Fatalf("ListSandboxesByNode: %v", err)
	}

	if gotPath != "/cube/sandbox/list" {
		t.Errorf("path: want /cube/sandbox/list, got %s", gotPath)
	}

	// Verify request body contains host_id.
	var req listSandboxesReq
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if req.HostID != "worker-01" {
		t.Errorf("host_id: want worker-01, got %s", req.HostID)
	}

	// Verify response parsing.
	if len(sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(sandboxes))
	}
	if sandboxes[0].SandboxID != "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6" {
		t.Errorf("sandbox[0] id: %s", sandboxes[0].SandboxID)
	}
	if sandboxes[1].SandboxID != "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6" {
		t.Errorf("sandbox[1] id: %s", sandboxes[1].SandboxID)
	}
}

func TestListSandboxesByNodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"node not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ListSandboxesByNode(context.Background(), "nonexistent-node")
	if err == nil {
		t.Fatal("expected error when ret_code != 0")
	}
	if !strings.Contains(err.Error(), "100501") {
		t.Errorf("error should contain ret_code: %v", err)
	}
}

func TestListSandboxesByNodeMissingRetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ListSandboxesByNode(context.Background(), "node-missing-ret")
	if err == nil {
		t.Fatal("expected error when ret envelope is missing")
	}
	if !strings.Contains(err.Error(), "missing CubeMaster ret envelope") {
		t.Fatalf("expected missing ret error, got %v", err)
	}
}

func TestResolveHostIDParsesSingleNodeEnvelope(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"ret": {"ret_code": 200, "ret_msg": "Success"},
			"data": {
				"node_id": "192.0.2.222",
				"host_ip": "192.0.2.222"
			}
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	hostID, err := c.ResolveHostID(context.Background(), "192.0.2.222")
	if err != nil {
		t.Fatalf("ResolveHostID: %v", err)
	}
	if gotPath != "/internal/meta/nodes/192.0.2.222" {
		t.Errorf("path: want /internal/meta/nodes/192.0.2.222, got %s", gotPath)
	}
	if hostID != "192.0.2.222" {
		t.Errorf("hostID: want 192.0.2.222, got %s", hostID)
	}
}

func TestResolveHostIDFallsBackToNodeList(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/internal/meta/nodes/worker-ip" {
			w.Write([]byte(`{"ret":{"ret_code":200},"data":{}}`))
			return
		}
		w.Write([]byte(`{
			"ret": {"ret_code": 200, "ret_msg": "Success"},
			"data": [
				{"node_id": "node-a", "host_ip": "10.0.0.1"},
				{"node_id": "node-b", "host_ip": "worker-ip"}
			]
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	hostID, err := c.ResolveHostID(context.Background(), "worker-ip")
	if err != nil {
		t.Fatalf("ResolveHostID: %v", err)
	}
	if hostID != "node-b" {
		t.Errorf("hostID: want node-b, got %s", hostID)
	}
	if len(paths) != 2 || paths[0] != "/internal/meta/nodes/worker-ip" || paths[1] != "/internal/meta/nodes" {
		t.Errorf("paths: %v", paths)
	}
}

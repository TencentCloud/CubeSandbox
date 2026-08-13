// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubemaster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, c.IsolateNode(context.Background(), "worker-01"))
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/internal/meta/nodes/worker-01/isolation", gotPath)
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
	require.NoError(t, c.UnisolateNode(context.Background(), "worker-01"))
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/internal/meta/nodes/worker-01/isolation", gotPath)
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
	require.NoError(t, c.PauseSandbox(context.Background(), "sandbox-abc", "cubebox", "req-1"))

	var req sandboxUpdateReq
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "pause", req.Action)
	assert.Equal(t, "sandbox-abc", req.SandboxID)
	assert.Equal(t, "cubebox", req.InstanceType)
	assert.Equal(t, "req-1", req.RequestID)
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
	require.NoError(t, c.ResumeSandbox(context.Background(), "sandbox-abc", "cubebox", "req-2"))

	var req sandboxUpdateReq
	json.Unmarshal(body, &req)
	assert.Equal(t, "resume", req.Action)
}

func TestClientErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.IsolateNode(context.Background(), "node-x")
	require.Error(t, err, "expected error on 500")
	assert.Contains(t, err.Error(), "500")
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
	require.NoError(t, c.IsolateNode(context.Background(), "node-auth"))
}

func TestClientCubeMasterRetCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"sandbox not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.PauseSandbox(context.Background(), "no-such-sandbox", "cubebox", "req-x")
	require.Error(t, err, "expected error when ret_code != 200")
	assert.Contains(t, err.Error(), "100501")
}

func TestIsolateNodeRetCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"node not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.IsolateNode(context.Background(), "missing-node")
	require.Error(t, err, "expected error when isolation ret_code != 200")
	assert.Contains(t, err.Error(), "100501")
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
			require.Error(t, err, "expected malformed response to fail")
			require.Contains(t, err.Error(), tc.want)
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
	require.NoError(t, c.PauseSandbox(context.Background(), "sandbox-already", "cubebox", "req-already"), "PauseSandbox should treat ret_code 130490 as success")
	require.NoError(t, c.ResumeSandbox(context.Background(), "sandbox-already", "cubebox", "req-already"), "ResumeSandbox should treat ret_code 130490 as success")
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
				{"sandbox_id": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", "status": 2, "instance_type": "microvm"},
				{"sandbox_id": "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6", "status": 2}
			]
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	sandboxes, err := c.ListSandboxesByNode(context.Background(), "worker-01")
	require.NoError(t, err)

	assert.Equal(t, "/cube/sandbox/list", gotPath)

	// Verify request body contains host_id.
	var req listSandboxesReq
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "worker-01", req.HostID)
	assert.True(t, req.AllInstanceTypes)

	// Verify response parsing.
	require.Len(t, sandboxes, 2)
	assert.Equal(t, "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", sandboxes[0].SandboxID)
	assert.Equal(t, "microvm", sandboxes[0].InstanceType)
	assert.Equal(t, "f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6", sandboxes[1].SandboxID)
}

func TestListSandboxesByNodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100501,"ret_msg":"node not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ListSandboxesByNode(context.Background(), "nonexistent-node")
	require.Error(t, err, "expected error when ret_code != 0")
	assert.Contains(t, err.Error(), "100501")
}

func TestListSandboxesByNodeMissingRetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ListSandboxesByNode(context.Background(), "node-missing-ret")
	require.Error(t, err, "expected error when ret envelope is missing")
	require.Contains(t, err.Error(), "missing CubeMaster ret envelope")
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
	require.NoError(t, err)
	assert.Equal(t, "/internal/meta/nodes/192.0.2.222", gotPath)
	assert.Equal(t, "192.0.2.222", hostID)
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
	require.NoError(t, err)
	assert.Equal(t, "node-b", hostID)
	assert.Equal(t, []string{"/internal/meta/nodes/worker-ip", "/internal/meta/nodes"}, paths)
}

// TestUnisolateNodeHTTPError verifies that UnisolateNode propagates an error
// when the server returns a non-200 HTTP status code.
func TestUnisolateNodeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.UnisolateNode(context.Background(), "worker-01")
	require.Error(t, err, "expected error when server returns 500")
	assert.Contains(t, err.Error(), "500", "error must mention HTTP status code")
}

// TestUnisolateNodeRetCodeError verifies that UnisolateNode returns an error
// when the HTTP status is 200 but the response ret_code indicates failure.
func TestUnisolateNodeRetCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":100404,"ret_msg":"node not found"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	err := c.UnisolateNode(context.Background(), "missing-node")
	require.Error(t, err, "expected error when ret_code != 200")
	assert.Contains(t, err.Error(), "100404")
}

// TestParseMetaNodeEnvelopedFormat verifies parseMetaNode handles the standard
// CubeMaster envelope format {"ret":{...},"data":{...}}.
func TestParseMetaNodeEnvelopedFormat(t *testing.T) {
	body := []byte(`{"ret":{"ret_code":200},"data":{"node_id":"n1","host_ip":"1.2.3.4"}}`)
	node, ok := parseMetaNode(body)
	require.True(t, ok, "expected parseMetaNode to succeed for enveloped format")
	assert.Equal(t, "n1", node.NodeID)
	assert.Equal(t, "1.2.3.4", node.HostIP)
}

// TestParseMetaNodeBareFormat verifies parseMetaNode handles the bare format
// {"node_id":"...","host_ip":"..."} with no ret envelope.
func TestParseMetaNodeBareFormat(t *testing.T) {
	body := []byte(`{"node_id":"n2","host_ip":"5.6.7.8"}`)
	node, ok := parseMetaNode(body)
	require.True(t, ok, "expected parseMetaNode to succeed for bare format")
	assert.Equal(t, "n2", node.NodeID)
	assert.Equal(t, "5.6.7.8", node.HostIP)
}

// TestParseMetaNodeInvalid verifies parseMetaNode returns (empty, false) when
// the body carries neither a valid node_id in data nor as a bare field.
func TestParseMetaNodeInvalid(t *testing.T) {
	body := []byte(`{"garbage": true}`)
	_, ok := parseMetaNode(body)
	assert.False(t, ok, "expected parseMetaNode to fail for invalid body")
}

// TestDoContextCancelled verifies that do (and IsolateNode) returns an error
// when the context is already cancelled before the request is made.
func TestDoContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before the call

	c := New(srv.URL, "", "", false)
	err := c.IsolateNode(ctx, "node-x")
	require.Error(t, err, "expected error when context is already cancelled")
}

// TestParseMetaNodeEnvelopedNon200Ret verifies that parseMetaNode returns false
// when the envelope is present but ret_code != 200.
func TestParseMetaNodeEnvelopedNon200Ret(t *testing.T) {
	body := []byte(`{"ret":{"ret_code":404,"ret_msg":"not found"},"data":{"node_id":"n1","host_ip":"1.2.3.4"}}`)
	_, ok := parseMetaNode(body)
	assert.False(t, ok, "expected parseMetaNode to return false when ret_code != 200")
}

// TestResolveHostIDNodeNotFound verifies that ResolveHostID returns an error
// when neither the direct lookup nor the list contains a matching node.
func TestResolveHostIDNodeNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path != "/internal/meta/nodes" {
			// Direct lookup returns empty data so fast path fails.
			w.Write([]byte(`{"ret":{"ret_code":200},"data":{}}`))
			return
		}
		// List returns nodes but none match the identifier.
		w.Write([]byte(`{"ret":{"ret_code":200},"data":[{"node_id":"node-a","host_ip":"10.0.0.1"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ResolveHostID(context.Background(), "unknown-identifier")
	require.Error(t, err, "expected error when node not found")
	assert.Contains(t, err.Error(), "node not found in CubeMaster")
}

// TestResolveHostIDListFails verifies that ResolveHostID returns an error when
// both the direct node lookup and the fallback list request fail.
func TestResolveHostIDListFails(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// First call (direct lookup): return HTTP 500 so do() returns error.
		// Second call (list): also return HTTP 500.
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ResolveHostID(context.Background(), "bad-node")
	require.Error(t, err, "expected error when both direct and list requests fail")
	assert.Contains(t, err.Error(), "list nodes")
}

// TestListSandboxesByNodeHTTPError verifies that ListSandboxesByNode propagates
// the error from do() when the server returns a non-200 HTTP status.
func TestListSandboxesByNodeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ListSandboxesByNode(context.Background(), "worker-01")
	require.Error(t, err, "expected error when server returns non-200")
	assert.Contains(t, err.Error(), "ListSandboxesByNode")
}

// TestResolveHostIDListUnmarshalError verifies that ResolveHostID returns an
// error when the fallback node list returns non-JSON.
func TestResolveHostIDListUnmarshalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path != "/internal/meta/nodes" {
			// Direct lookup: empty data so fast path fails.
			w.Write([]byte(`{"ret":{"ret_code":200},"data":{}}`))
			return
		}
		// List returns invalid JSON.
		w.Write([]byte(`not-valid-json`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", false)
	_, err := c.ResolveHostID(context.Background(), "some-node")
	require.Error(t, err, "expected error when node list returns invalid JSON")
	assert.Contains(t, err.Error(), "unmarshal")
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// hostPort splits an httptest server address into host and port.
func hostPort(t *testing.T, srv *httptest.Server) (string, string) {
	t.Helper()
	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return h, p
}

var quotaGetFlags = []cli.Flag{cli.BoolFlag{Name: "json"}}

var quotaSetFlags = []cli.Flag{
	cli.Int64Flag{Name: "mcpu"},
	cli.StringFlag{Name: "mem"},
	cli.Int64Flag{Name: "mvm"},
	cli.Int64Flag{Name: "create-concurrent"},
	cli.Float64Flag{Name: "paused-ratio"},
	cli.Int64Flag{Name: "revision"},
	cli.StringFlag{Name: "operator", Value: "cli"},
	cli.BoolFlag{Name: "json"},
}

var quotaHistoryFlags = []cli.Flag{cli.IntFlag{Name: "limit", Value: 50}, cli.BoolFlag{Name: "json"}}

func TestQuotaGet_Table(t *testing.T) {
	view := &model.QuotaView{
		NodeID: "node-1",
		Spec:   &model.QuotaSpec{MCpuLimit: 128000, MemLimit: "262144Mi", MvmLimit: 500, CreationConcurrentNum: 32, Revision: 7, NodeManaged: true},
		Actual: model.QuotaActual{MilliCPU: 64000, MemMB: 131072, MaxMvmNum: 500, CreateConcurrentNum: 32},
		Drift:  model.DriftDetected,
	}
	srv, h, p := setupStubServer(t, 200, view)
	defer srv.Close()
	ctx := newCommandContext(t, "get", quotaGetFlags, h, p, []string{"node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaGetAction(ctx)) })
	assert.Contains(t, out, "node-1")
	assert.Contains(t, out, "mcpu=64000")
	assert.Contains(t, out, "mem=131072Mi")
	assert.Contains(t, out, "maxMvm=500")
	assert.Contains(t, out, "detected")
	assert.NotContains(t, out, "SPEC")
}

func TestQuotaGet_JSON(t *testing.T) {
	view := &model.QuotaView{NodeID: "node-1", Drift: model.DriftNoSpec}
	srv, h, p := setupStubServer(t, 200, view)
	defer srv.Close()
	ctx := newCommandContext(t, "get", quotaGetFlags, h, p, []string{"--json", "node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaGetAction(ctx)) })
	assert.Contains(t, out, `"node_id": "node-1"`)
}

func TestQuotaGet_NoArgs(t *testing.T) {
	ctx := newCommandContext(t, "get", quotaGetFlags, "127.0.0.1", "3010", nil)
	err := quotaGetAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node id is required")
}

func TestQuotaGet_HTTPError(t *testing.T) {
	srv, h, p := setupStubServer(t, 404, "node not found")
	defer srv.Close()
	ctx := newCommandContext(t, "get", quotaGetFlags, h, p, []string{"node-1"})
	err := quotaGetAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestQuotaSet_Applied(t *testing.T) {
	var gotReq struct {
		View model.QuotaView  `json:"view"`
		Push model.PushResult `json:"push"`
	}
	gotReq.View = model.QuotaView{NodeID: "node-1", Spec: &model.QuotaSpec{Revision: 8}}
	gotReq.Push = model.PushResult{Applied: true}

	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gotReq)
	}))
	defer srv.Close()
	host, portVal := hostPort(t, srv)
	serverList = []string{host}
	port = portVal

	ctx := newCommandContext(t, "set", quotaSetFlags, host, portVal,
		[]string{"--mcpu", "128000", "--mem", "262144Mi", "--operator", "bob", "node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaSetAction(ctx)) })

	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/internal/v1/nodes/node-1/config/quota", gotPath)
	assert.Contains(t, gotQuery, "operator=bob")
	assert.Contains(t, gotBody, `"mcpu_limit":128000`)
	assert.Contains(t, gotBody, `"mem_limit":"262144Mi"`)
	assert.Contains(t, out, "quota updated and applied")
	assert.NotContains(t, out, "revision=")
}

func TestQuotaSet_PushSkipped(t *testing.T) {
	resp := struct {
		View model.QuotaView  `json:"view"`
		Push model.PushResult `json:"push"`
	}{View: model.QuotaView{NodeID: "node-1"}, Push: model.PushResult{SkipReason: "ops-agent integration disabled"}}
	srv, h, p := setupStubServer(t, 200, resp)
	defer srv.Close()
	ctx := newCommandContext(t, "set", quotaSetFlags, h, p, []string{"--mvm", "500", "node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaSetAction(ctx)) })
	assert.Contains(t, out, "not applied yet")
	assert.Contains(t, out, "ops-agent integration disabled")
}

func TestQuotaSet_NoFields(t *testing.T) {
	ctx := newCommandContext(t, "set", quotaSetFlags, "127.0.0.1", "3010", []string{"node-1"})
	err := quotaSetAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to set")
}

func TestQuotaHistory_Table(t *testing.T) {
	entries := []model.QuotaHistoryEntry{
		{Operator: "cli", Detail: `{"new":{"mcpu_limit":64000}}`},
	}
	srv, h, p := setupStubServer(t, 200, entries)
	defer srv.Close()
	ctx := newCommandContext(t, "history", quotaHistoryFlags, h, p, []string{"node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaHistoryAction(ctx)) })
	assert.Contains(t, out, "OPERATOR")
	assert.Contains(t, out, "cli")
}

func TestQuotaHistory_Empty(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, []model.QuotaHistoryEntry{})
	defer srv.Close()
	ctx := newCommandContext(t, "history", quotaHistoryFlags, h, p, []string{"node-1"})
	out := captureStdout(t, func() { assert.NoError(t, quotaHistoryAction(ctx)) })
	assert.Contains(t, out, "no set-quota history")
}

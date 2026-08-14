// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// stubServer starts an httptest.Server returning the given status + body.
func stubServer(t *testing.T, status int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status >= 400 {
			http.Error(w, body.(string), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(body)
		_, _ = w.Write(data)
	}))
}

// setupStubServer starts a test server and wires package-level serverList/port.
// Returns the server, host, and port for --address/--port flags.
func setupStubServer(t *testing.T, status int, body interface{}) (*httptest.Server, string, string) {
	t.Helper()
	srv := stubServer(t, status, body)
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	serverList = []string{host}
	port = p
	return srv, host, p
}

// newCommandContext builds a cli.Context with global + command flags set.
func newCommandContext(t *testing.T, name string, cmdFlags []cli.Flag, address, portVal string, args []string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	for _, f := range append(globalFlags(), cmdFlags...) {
		f.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args: %v", err)
	}
	_ = set.Set("address", address)
	_ = set.Set("port", portVal)
	app := cli.NewApp()
	app.Flags = globalFlags()
	return cli.NewContext(app, set, nil)
}

var listFlags = []cli.Flag{
	cli.BoolFlag{Name: "json"},
	cli.StringFlag{Name: "hostid"},
	cli.BoolFlag{Name: "score-only"},
}

var isolateFlags = []cli.Flag{cli.BoolFlag{Name: "json"}}

func TestListAction_JSON(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleSchedulerNodes())
	defer srv.Close()
	ctx := newCommandContext(t, "list", listFlags, h, p, []string{"--json"})
	assert.NoError(t, listAction(ctx))
}

func TestListAction_TableOutput(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleSchedulerNodes())
	defer srv.Close()
	ctx := newCommandContext(t, "list", listFlags, h, p, nil)
	out := captureStdout(t, func() { assert.NoError(t, listAction(ctx)) })
	assert.Contains(t, out, "node-1")
	assert.Contains(t, out, "node-2")
}

func TestListAction_HostID(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, sampleSchedulerNodes()[0])
	defer srv.Close()
	ctx := newCommandContext(t, "list", listFlags, h, p, []string{"--hostid", "node-1"})
	assert.NoError(t, listAction(ctx))
}

func TestListAction_HTTPError(t *testing.T) {
	srv, h, p := setupStubServer(t, 500, "internal error")
	defer srv.Close()
	ctx := newCommandContext(t, "list", listFlags, h, p, nil)
	err := listAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestIsolate_Success(t *testing.T) {
	snap := &model.NodeSnapshot{NodeID: "node-1", SchedulingDisabled: true}
	srv, h, p := setupStubServer(t, 200, snap)
	defer srv.Close()
	ctx := newCommandContext(t, "isolate", isolateFlags, h, p, []string{"node-1"})
	out := captureStdout(t, func() { assert.NoError(t, doIsolation(ctx, http.MethodPut)) })
	assert.Contains(t, out, "node-1")
	assert.Contains(t, out, "isolated")
	assert.Contains(t, out, "scheduling_disabled=true")
}

func TestUnisolate_Success(t *testing.T) {
	snap := &model.NodeSnapshot{NodeID: "node-1", SchedulingDisabled: false}
	srv, h, p := setupStubServer(t, 200, snap)
	defer srv.Close()
	ctx := newCommandContext(t, "unisolate", isolateFlags, h, p, []string{"node-1"})
	out := captureStdout(t, func() { assert.NoError(t, doIsolation(ctx, http.MethodDelete)) })
	assert.Contains(t, out, "node-1")
	assert.Contains(t, out, "unisolated")
	assert.Contains(t, out, "scheduling_disabled=false")
}

func TestIsolate_NoArgs(t *testing.T) {
	ctx := newCommandContext(t, "isolate", isolateFlags, "127.0.0.1", "3010", nil)
	err := doIsolation(ctx, http.MethodPut)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node id is required")
}

func TestIsolate_HTTPError(t *testing.T) {
	srv, h, p := setupStubServer(t, 500, "internal error")
	defer srv.Close()
	ctx := newCommandContext(t, "isolate", isolateFlags, h, p, []string{"node-1"})
	err := doIsolation(ctx, http.MethodPut)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node-1")
	assert.Contains(t, err.Error(), "500")
}

func TestIsolate_MultiNode(t *testing.T) {
	snap := &model.NodeSnapshot{NodeID: "node-1", SchedulingDisabled: true}
	srv, h, p := setupStubServer(t, 200, snap)
	defer srv.Close()
	ctx := newCommandContext(t, "isolate", isolateFlags, h, p, []string{"node-1", "node-2"})
	assert.NoError(t, doIsolation(ctx, http.MethodPut))
}

func TestDoHttpReq_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	ctx := newCommandContext(t, "test", nil, "127.0.0.1", "3010", []string{"--timeout", "50ms"})
	err := doHttpReq(ctx, srv.URL+"/x", http.MethodGet, "req-1", nil, nil)
	assert.Error(t, err)
}

func TestDoHttpReq_EmptyBody(t *testing.T) {
	srv, h, p := setupStubServer(t, 200, map[string]string{"ok": "1"})
	defer srv.Close()
	ctx := newCommandContext(t, "test", nil, h, p, nil)
	var rsp map[string]string
	err := doHttpReq(ctx, buildURL(serverList[0], "/x"), http.MethodGet, "req-1", nil, &rsp)
	assert.NoError(t, err)
	assert.Equal(t, "1", rsp["ok"])
}

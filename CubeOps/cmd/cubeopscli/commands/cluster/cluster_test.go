// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

var defaultsGetFlags = []cli.Flag{cli.BoolFlag{Name: "json"}}
var defaultsSetFlags = []cli.Flag{
	cli.StringFlag{Name: "operator", Value: "cli"},
	cli.BoolFlag{Name: "json"},
}

func hostPort(t *testing.T, srv *httptest.Server) (string, string) {
	t.Helper()
	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return h, p
}

func newContext(t *testing.T, cmdFlags []cli.Flag, address, port string, args []string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	global := []cli.Flag{
		cli.StringFlag{Name: "a, address", Value: "127.0.0.1"},
		cli.StringFlag{Name: "p, port", Value: "3010"},
		cli.DurationFlag{Name: "timeout", Value: 35 * time.Second},
	}
	for _, f := range append(global, cmdFlags...) {
		f.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	_ = set.Set("address", address)
	_ = set.Set("port", port)
	app := cli.NewApp()
	app.Flags = global
	return cli.NewContext(app, set, nil)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() { data, _ := io.ReadAll(r); done <- string(data) }()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func writeJSONBody(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func TestDefaultsGet_Unset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/internal/v1/cluster/quota-defaults", r.URL.Path)
		writeJSONBody(t, w, model.ClusterQuotaDefaults{})
	}))
	defer srv.Close()
	host, port := hostPort(t, srv)
	ctx := newContext(t, defaultsGetFlags, host, port, nil)
	out := captureStdout(t, func() { assert.NoError(t, defaultsGetAction(ctx)) })
	assert.Contains(t, out, "unset")
}

func TestDefaultsGet_Set(t *testing.T) {
	v := 0.5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, model.ClusterQuotaDefaults{PausedReleaseRatio: &v})
	}))
	defer srv.Close()
	host, port := hostPort(t, srv)
	ctx := newContext(t, defaultsGetFlags, host, port, nil)
	out := captureStdout(t, func() { assert.NoError(t, defaultsGetAction(ctx)) })
	assert.Contains(t, out, "0.5")
}

func TestDefaultsSet_Value(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		writeJSONBody(t, w, map[string]interface{}{
			"defaults":    map[string]interface{}{"paused_release_ratio": 0.5},
			"propagation": map[string]interface{}{"bumped": 3, "pushed": 2, "push_failed": 1},
		})
	}))
	defer srv.Close()
	host, port := hostPort(t, srv)
	ctx := newContext(t, defaultsSetFlags, host, port, []string{"0.5"})
	out := captureStdout(t, func() { assert.NoError(t, defaultsSetAction(ctx)) })

	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/internal/v1/cluster/quota-defaults", gotPath)
	assert.Contains(t, gotQuery, "operator=cli")
	assert.Contains(t, gotBody, `"paused_release_ratio":0.5`)
	assert.Contains(t, out, "cluster default saved")
	assert.Contains(t, out, "bumped=3 pushed=2 push_failed=1")
}

func TestDefaultsSet_Clear(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		writeJSONBody(t, w, map[string]interface{}{
			"defaults":    map[string]interface{}{},
			"propagation": map[string]interface{}{"bumped": 0},
		})
	}))
	defer srv.Close()
	host, port := hostPort(t, srv)
	ctx := newContext(t, defaultsSetFlags, host, port, []string{"clear"})
	out := captureStdout(t, func() { assert.NoError(t, defaultsSetAction(ctx)) })
	assert.Contains(t, gotBody, `"paused_release_ratio":null`)
	assert.Contains(t, out, "cleared")
}

func TestDefaultsSet_InvalidValue(t *testing.T) {
	ctx := newContext(t, defaultsSetFlags, "127.0.0.1", "3010", []string{"abc"})
	err := defaultsSetAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ratio")
}

func TestDefaultsSet_MissingArg(t *testing.T) {
	ctx := newContext(t, defaultsSetFlags, "127.0.0.1", "3010", nil)
	err := defaultsSetAction(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one argument")
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func TestTemplateBackendCLIRequest(t *testing.T) {
	oldServers, oldPort := serverList, port
	t.Cleanup(func() { serverList, port = oldServers, oldPort })
	for _, command := range []string{"create", "build"} {
		for _, backend := range []string{"xfs", "s3", "jfs", "cos", "minio", "cow", "fs", "unsupported"} {
			t.Run(command+"/"+backend, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var body map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if backend != "xfs" && backend != "s3" && backend != "jfs" {
						t.Error("invalid backend reached HTTP")
						return
					}
					want := backend
					if body["backend"] != want {
						t.Errorf("sent backend=%v, want %s", body["backend"], want)
					}
					if command == "create" {
						annotations, _ := body["annotations"].(map[string]interface{})
						if annotations[constants.CubeAnnotationStorageBackend] != want {
							t.Error("request annotation was not overridden")
						}
					}
					fmt.Fprint(w, `{"ret":{"ret_code":200},"template_id":"tpl-test"}`)
				}))
				defer server.Close()
				host, serverPort, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				app := cli.NewApp()
				app.Flags = []cli.Flag{cli.StringFlag{Name: "address"}, cli.StringFlag{Name: "port"}, cli.DurationFlag{Name: "timeout", Value: time.Second}}
				app.Commands = []cli.Command{TemplateCommand}
				args := []string{"cubemastercli", "--address", host, "--port", serverPort, "tpl", command, "--backend", backend}
				if command == "create" {
					file := t.TempDir() + "/request.json"
					if err := os.WriteFile(file, []byte(`{"backend":"s3","annotations":{"cube.master.storage.backend":"s3"}}`), 0o600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--file", file)
				} else {
					args = append(args, "--image", "example/image:test", "--writable-layer-size", "1Gi", "--detach")
				}
				err = app.Run(args)
				if backend != "xfs" && backend != "s3" && backend != "jfs" {
					if err == nil || calls != 0 {
						t.Fatalf("invalid backend reached HTTP: error=%v calls=%d", err, calls)
					}
				} else if err != nil || calls != 1 {
					t.Fatalf("CLI request failed: error=%v calls=%d", err, calls)
				}
			})
		}
	}
}

func TestApplyTemplateBackendFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "s3"}, {[]string{"--backend", "xfs"}, "xfs"},
		{[]string{"--backend", "S3"}, "s3"}, {[]string{"--backend", " XFS "}, "xfs"},
	} {
		req := &types.CreateCubeSandboxReq{Backend: "s3", Annotations: map[string]string{constants.CubeAnnotationStorageBackend: "s3"}}
		if err := applyTemplateBackendFlag(newCreateContext(t, tc.args), req); err != nil {
			t.Fatal(err)
		}
		if req.Backend != tc.want || req.Annotations[constants.CubeAnnotationStorageBackend] != tc.want {
			t.Fatalf("args=%v: backend=%s annotations=%v", tc.args, req.Backend, req.Annotations)
		}
	}
	for _, backend := range []string{"", "cos", "minio", "cow", "fs", "unsupported-store"} {
		if _, err := normalizeCLIBackend(backend); err == nil {
			t.Fatalf("unsupported backend %q accepted", backend)
		}
	}
	if !reflect.DeepEqual(TemplateCreateFromImageCommand.Aliases, []string{"build"}) {
		t.Fatal("tpl build alias missing")
	}
}

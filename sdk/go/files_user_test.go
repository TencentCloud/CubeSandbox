// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileRequestOptions(t *testing.T) {
	if got := resolveFileRequestOptions(); got.user != "" {
		t.Fatalf("default user=%q, want empty", got.user)
	}
	if got := resolveFileRequestOptions(withUser("root"), nil, withUser("app")); got.user != "app" {
		t.Fatalf("resolved user=%q, want last option to win", got.user)
	}
}

func TestFilesForUserReturnsImmutableView(t *testing.T) {
	reader := &fakeFileReader{content: "content"}
	files := &Files{reader: reader}

	rootFiles := files.ForUser("root")
	appFiles := rootFiles.ForUser("app")

	if files == rootFiles || rootFiles == appFiles {
		t.Fatal("ForUser must return a new filesystem view")
	}
	if files.user != "" || rootFiles.user != "root" || appFiles.user != "app" {
		t.Fatalf("users=%q/%q/%q", files.user, rootFiles.user, appFiles.user)
	}

	if _, err := appFiles.Read(context.Background(), "/tmp/file"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if reader.path != "/tmp/file" || reader.user != "app" {
		t.Fatalf("read path/user=%q/%q", reader.path, reader.user)
	}

	if (*Files)(nil).ForUser("root") != nil {
		t.Fatal("ForUser on a nil Files must return nil")
	}
}

type capturedFileRequest struct {
	method        string
	path          string
	contentType   string
	username      string
	authorization string
	accessToken   string
	e2bToken      string
	cubeToken     string
	body          string
}

func TestFilesForUserPropagatesIdentityToEveryTransport(t *testing.T) {
	var mu sync.Mutex
	var requests []capturedFileRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, capturedFileRequest{
			method:        r.Method,
			path:          r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			username:      r.URL.Query().Get("username"),
			authorization: r.Header.Get("Authorization"),
			accessToken:   r.Header.Get("X-Access-Token"),
			e2bToken:      r.Header.Get("e2b-traffic-access-token"),
			cubeToken:     r.Header.Get("cube-traffic-access-token"),
			body:          string(body),
		})
		mu.Unlock()

		switch r.URL.Path {
		case "/files":
			if r.Method == http.MethodGet {
				fmt.Fprint(w, "content")
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/filesystem.Filesystem/ListDir":
			fmt.Fprint(w, `{"entries":[]}`)
		case "/filesystem.Filesystem/Stat":
			fmt.Fprint(w, `{"entry":{"name":"file","type":"FILE_TYPE_FILE","path":"/tmp/file","size":"1","mode":420}}`)
		case "/filesystem.Filesystem/Remove":
			fmt.Fprint(w, `{}`)
		case "/filesystem.Filesystem/Move":
			fmt.Fprint(w, `{"entry":{"name":"renamed","type":"FILE_TYPE_FILE","path":"/tmp/renamed","size":"1","mode":420}}`)
		case "/filesystem.Filesystem/MakeDir":
			fmt.Fprint(w, `{"entry":{"name":"dir","type":"FILE_TYPE_DIRECTORY","path":"/tmp/dir","size":"0","mode":493}}`)
		case "/filesystem.Filesystem/WatchDir":
			w.Header().Set("Content-Type", connectContentType)
			_, _ = w.Write(connectFrame(0, []byte(`{"start":{}}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{
		ProxyNodeIP:    host,
		ProxyPortHTTP:  port,
		SandboxDomain:  "cube.test",
		RequestTimeout: time.Second,
	})
	sb := &Sandbox{
		client:             client,
		SandboxID:          "sb-files-user",
		EnvdAccessToken:    "envd-token",
		TrafficAccessToken: "traffic-token",
	}
	files := sb.Files().ForUser("app")
	ctx := context.Background()

	if _, err := files.Read(ctx, "/tmp/file"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := files.Write(ctx, "/tmp/file", []byte("content")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n, err := files.WriteFiles(ctx, []WriteEntry{
		{Path: "/tmp/a", Data: []byte("a")},
		{Path: "/tmp/b", Data: []byte("b")},
	}); err != nil || n != 2 {
		t.Fatalf("WriteFiles: n=%d err=%v", n, err)
	}
	if _, err := files.List(ctx, "/tmp"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := files.Stat(ctx, "/tmp/file"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if exists, err := files.Exists(ctx, "/tmp/file"); err != nil || !exists {
		t.Fatalf("Exists: exists=%v err=%v", exists, err)
	}
	if err := files.Remove(ctx, "/tmp/file"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := files.Rename(ctx, "/tmp/file", "/tmp/renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := files.MakeDir(ctx, "/tmp/dir"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	watcher, err := files.WatchDir(ctx, "/tmp")
	if err != nil {
		t.Fatalf("WatchDir: %v", err)
	}
	for range watcher.Events {
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("close watcher: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 11 {
		t.Fatalf("request count=%d, want 11: %#v", len(requests), requests)
	}
	for _, request := range requests {
		if request.accessToken != "envd-token" || request.e2bToken != "traffic-token" || request.cubeToken != "traffic-token" {
			t.Errorf("%s tokens=%q/%q/%q", request.path, request.accessToken, request.e2bToken, request.cubeToken)
		}
		if request.path == "/files" {
			if request.username != "app" {
				t.Errorf("%s %s username=%q, want app", request.method, request.path, request.username)
			}
			if request.authorization != "" {
				t.Errorf("%s %s Authorization=%q, want empty", request.method, request.path, request.authorization)
			}
			continue
		}
		if request.username != "" {
			t.Errorf("%s username=%q, want empty", request.path, request.username)
		}
		wantContentType := "application/json"
		if request.path == "/filesystem.Filesystem/WatchDir" {
			wantContentType = connectContentType
		}
		if request.contentType != wantContentType {
			t.Errorf("%s Content-Type=%q, want %q", request.path, request.contentType, wantContentType)
		}
		if request.authorization != basicAuthUser("app") {
			t.Errorf("%s Authorization=%q, want %q", request.path, request.authorization, basicAuthUser("app"))
		}
		if strings.Contains(request.body, `"user"`) || strings.Contains(request.body, `"username"`) {
			t.Errorf("%s request body contains user identity: %s", request.path, request.body)
		}
	}
}

func TestFilesWithoutUserPreservesUnscopedRequestShape(t *testing.T) {
	var mu sync.Mutex
	var requests []capturedFileRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, capturedFileRequest{
			path:          r.URL.Path,
			username:      r.URL.Query().Get("username"),
			authorization: r.Header.Get("Authorization"),
		})
		mu.Unlock()
		if r.URL.Path == "/files" {
			fmt.Fprint(w, "content")
			return
		}
		fmt.Fprint(w, `{"entries":[]}`)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{ProxyNodeIP: host, ProxyPortHTTP: port, SandboxDomain: "cube.test", RequestTimeout: time.Second})
	sb := &Sandbox{client: client, SandboxID: "sb-files-unscoped"}

	if _, err := sb.Files().Read(context.Background(), "/tmp/file"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := sb.Files().List(context.Background(), "/tmp"); err != nil {
		t.Fatalf("List: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("request count=%d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.username != "" || request.authorization != "" {
			t.Errorf("%s username/Authorization=%q/%q, want empty", request.path, request.username, request.authorization)
		}
	}
}

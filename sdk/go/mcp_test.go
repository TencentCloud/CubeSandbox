// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMCPBackend struct {
	mu          sync.Mutex
	createBody  map[string]any
	process     map[string]any
	authUser    string
	deleted     []string
	exitCode    int
	stderr      string
	createCode  int
	tokenExists bool
	missing     map[string]bool
	legacy      bool
	posted      []string
}

func (f *fakeMCPBackend) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			f.createBody = nil
			if err := json.Unmarshal(body, &f.createBody); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			tpl, _ := f.createBody["templateID"].(string)
			f.posted = append(f.posted, tpl)
			if f.missing[tpl] {
				if f.legacy {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprintf(w, `{"code":500,"message":"CubeMaster returned error code 130404: failed to resolve template identifier %q: template not found"}`, tpl)
					return
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, `{"code":404,"message":"template %s not found: template not found"}`, tpl)
				return
			}
			if f.createCode != 0 {
				w.WriteHeader(f.createCode)
				_, _ = w.Write([]byte(`{"code":400,"message":"mcp must be an object keyed by MCP server name"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"sb-mcp","templateID":"mcp-gateway","domain":"cube.test"}`))
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/process.Process/Start":
			var payload map[string]any
			if err := json.Unmarshal(body[5:], &payload); err != nil {
				t.Errorf("decode process payload: %v", err)
			}
			f.process = payload["process"].(map[string]any)
			f.authUser = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", connectContentType)
			_, _ = w.Write(connectEnvelope(0, `{"event":{"start":{"pid":7}}}`))
			if f.stderr != "" {
				_, _ = w.Write(connectEnvelope(0, fmt.Sprintf(`{"event":{"data":{"stderr":%q}}}`,
					base64.StdEncoding.EncodeToString([]byte(f.stderr)))))
			}
			_, _ = w.Write(connectEnvelope(0, fmt.Sprintf(`{"event":{"end":{"exitCode":%d,"exited":true}}}`, f.exitCode)))
			_, _ = w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
		case r.URL.Path == "/filesystem.Filesystem/Stat":
			if !f.tokenExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found","message":"no such file"}`))
				return
			}
			_, _ = w.Write([]byte(`{"entry":{"name":".token","path":"/etc/mcp-gateway/.token"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/files":
			if got := r.URL.Query().Get("path"); got != MCPTokenPath {
				t.Errorf("read path = %q", got)
			}
			_, _ = w.Write([]byte("file-token\n"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newMCPTestClient(t *testing.T, backend *fakeMCPBackend, cfg Config) *Client {
	t.Helper()
	server := httptest.NewServer(backend.handler(t))
	t.Cleanup(server.Close)
	host, port := serverHostPort(t, server.URL)
	cfg.APIURL = server.URL
	cfg.ProxyNodeIP = host
	cfg.ProxyPortHTTP = port
	cfg.SandboxDomain = "cube.test"
	cfg.RequestTimeout = 5 * time.Second
	return NewClient(cfg)
}

var testMCP = MCPServers{
	"duckduckgo":        map[string]any{},
	"github/acme/tools": GitHubMCPServer{RunCmd: "echo 'hi'", Envs: map[string]string{"K": "v"}},
}

func TestCreateWithMCPStartsGateway(t *testing.T) {
	backend := &fakeMCPBackend{}
	client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})

	sb, err := client.Create(context.Background(), CreateOptions{MCP: testMCP})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if backend.createBody["templateID"] != DefaultMCPTemplateID {
		t.Errorf("templateID = %v, want %s", backend.createBody["templateID"], DefaultMCPTemplateID)
	}
	mcp, _ := backend.createBody["mcp"].(map[string]any)
	if gh, _ := mcp["github/acme/tools"].(map[string]any); gh["runCmd"] != "echo 'hi'" || mcp["duckduckgo"] == nil {
		t.Errorf("mcp payload = %#v", backend.createBody["mcp"])
	}

	args := backend.process["args"].([]any)
	cmd := args[2].(string)
	wantJSON, _ := json.Marshal(testMCP)
	if want := "mcp-gateway --config " + shellQuote(string(wantJSON)); cmd != want {
		t.Errorf("command = %q\nwant      %q", cmd, want)
	}
	envs := backend.process["envs"].(map[string]any)
	token, _ := envs["GATEWAY_ACCESS_TOKEN"].(string)
	if len(token) != 36 {
		t.Errorf("token = %q", token)
	}
	if backend.authUser != basicAuthUser("root") {
		t.Errorf("gateway must run as root, auth = %q", backend.authUser)
	}
	got, err := sb.GetMCPToken(context.Background())
	if err != nil || got != token {
		t.Errorf("GetMCPToken = %q, %v; want cached %q", got, err, token)
	}
	if len(backend.deleted) != 0 {
		t.Errorf("sandbox must not be killed on success: %v", backend.deleted)
	}
}

func TestCreateWithMCPTemplateSelection(t *testing.T) {
	backend := &fakeMCPBackend{}
	client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default", MCPTemplateID: "tpl-mcp"})
	if _, err := client.Create(context.Background(), CreateOptions{MCP: MCPServers{}}); err != nil {
		t.Fatal(err)
	}
	if backend.createBody["templateID"] != "tpl-mcp" {
		t.Errorf("templateID = %v", backend.createBody["templateID"])
	}
	if _, err := client.Create(context.Background(), CreateOptions{TemplateID: "tpl-x", MCP: MCPServers{}}); err != nil {
		t.Fatal(err)
	}
	if backend.createBody["templateID"] != "tpl-x" {
		t.Errorf("explicit templateID = %v", backend.createBody["templateID"])
	}
	backend.process = nil
	if _, err := client.Create(context.Background(), CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if backend.createBody["templateID"] != "tpl-default" || backend.createBody["mcp"] != nil || backend.process != nil {
		t.Errorf("create without MCP: body=%v process=%v", backend.createBody, backend.process)
	}
}

func TestCreateWithMCPGatewayFailureKillsSandbox(t *testing.T) {
	backend := &fakeMCPBackend{exitCode: 1, stderr: `mcp server "duckduckgo": boom`}
	client := newMCPTestClient(t, backend, Config{})
	sb, err := client.Create(context.Background(), CreateOptions{MCP: testMCP})
	if sb != nil || !errors.Is(err, ErrMCPGateway) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Create = %v, %v", sb, err)
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != "/sandboxes/sb-mcp" {
		t.Fatalf("deleted = %v", backend.deleted)
	}
}

func TestCreateWithInvalidMCPReturnsAPIError(t *testing.T) {
	backend := &fakeMCPBackend{createCode: http.StatusBadRequest}
	client := newMCPTestClient(t, backend, Config{})
	_, err := client.Create(context.Background(), CreateOptions{MCP: MCPServers{"x": "bad"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	if backend.process != nil {
		t.Error("gateway must not start after a rejected create")
	}
}

func TestGetMCPURLAndTokenFromSandbox(t *testing.T) {
	backend := &fakeMCPBackend{tokenExists: true}
	client := newMCPTestClient(t, backend, Config{})
	sb := &Sandbox{SandboxID: "sb-mcp"}
	client.attachSandbox(sb)
	if got := sb.GetMCPURL(); got != "http://50005-sb-mcp.cube.test/mcp" {
		t.Errorf("GetMCPURL = %q", got)
	}
	token, err := sb.GetMCPToken(context.Background())
	if err != nil || token != "file-token" {
		t.Fatalf("GetMCPToken = %q, %v", token, err)
	}

	backend.tokenExists = false
	fresh := &Sandbox{SandboxID: "sb-mcp"}
	client.attachSandbox(fresh)
	if token, err := fresh.GetMCPToken(context.Background()); err != nil || token != "" {
		t.Fatalf("GetMCPToken without gateway = %q, %v", token, err)
	}

	https := NewClient(Config{ProxyScheme: "https", SandboxDomain: "cube.test"})
	secure := &Sandbox{SandboxID: "sb-mcp"}
	https.attachSandbox(secure)
	if got := secure.GetMCPURL(); got != "https://50005-sb-mcp.cube.test/mcp" {
		t.Errorf("https GetMCPURL = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`{"a":"it's"}`); got != `'{"a":"it'\''s"}'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

func TestCreateWithMCPFallsBackToDefaultTemplate(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		backend := &fakeMCPBackend{missing: map[string]bool{DefaultMCPTemplateID: true}, legacy: legacy}
		client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})
		if _, err := client.Create(context.Background(), CreateOptions{MCP: testMCP}); err != nil {
			t.Fatalf("legacy=%v: Create: %v", legacy, err)
		}
		if got := strings.Join(backend.posted, ","); got != "mcp-gateway,tpl-default" {
			t.Errorf("legacy=%v: posted templates = %s", legacy, got)
		}
		if backend.process == nil {
			t.Errorf("legacy=%v: gateway not started on the fallback template", legacy)
		}
	}
}

func TestCreateWithMCPFailsWhenNoTemplateExists(t *testing.T) {
	backend := &fakeMCPBackend{missing: map[string]bool{DefaultMCPTemplateID: true, "tpl-default": true}}
	client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})
	_, err := client.Create(context.Background(), CreateOptions{MCP: testMCP})
	if !errors.Is(err, ErrTemplateNotFound) || !strings.Contains(err.Error(), `["mcp-gateway" "tpl-default"]`) {
		t.Fatalf("err = %v", err)
	}
	if backend.process != nil {
		t.Error("gateway must not start without a template")
	}
}

func TestCreateWithMCPDoesNotFallBackFromExplicitTemplate(t *testing.T) {
	backend := &fakeMCPBackend{missing: map[string]bool{"tpl-x": true}}
	client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})
	_, err := client.Create(context.Background(), CreateOptions{TemplateID: "tpl-x", MCP: testMCP})
	if !errors.Is(err, ErrTemplateNotFound) || strings.Join(backend.posted, ",") != "tpl-x" {
		t.Fatalf("err = %v, posted = %v", err, backend.posted)
	}
}

func TestCreateWithMCPKeepsExtraTemplateID(t *testing.T) {
	backend := &fakeMCPBackend{}
	client := newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})
	if _, err := client.Create(context.Background(), CreateOptions{MCP: testMCP, Extra: map[string]any{"templateID": 42}}); err != nil {
		t.Fatalf("Create with non-string Extra templateID: %v", err)
	}
	if got := backend.createBody["templateID"]; got != float64(42) || len(backend.posted) != 1 {
		t.Errorf("templateID = %#v, posted = %v", got, backend.posted)
	}

	backend = &fakeMCPBackend{missing: map[string]bool{"tpl-extra": true}}
	client = newMCPTestClient(t, backend, Config{TemplateID: "tpl-default"})
	_, err := client.Create(context.Background(), CreateOptions{MCP: testMCP, Extra: map[string]any{"templateID": "tpl-extra"}})
	if !errors.Is(err, ErrTemplateNotFound) || strings.Join(backend.posted, ",") != "tpl-extra" {
		t.Fatalf("err = %v, posted = %v", err, backend.posted)
	}
}

func TestCreateWithMCPNamesTemplateWithoutGateway(t *testing.T) {
	backend := &fakeMCPBackend{exitCode: 127, stderr: "/bin/bash: mcp-gateway: command not found"}
	client := newMCPTestClient(t, backend, Config{})
	_, err := client.Create(context.Background(), CreateOptions{MCP: testMCP})
	if !errors.Is(err, ErrMCPGateway) || !strings.Contains(err.Error(), `template "mcp-gateway" does not provide mcp-gateway`) {
		t.Fatalf("err = %v", err)
	}
}

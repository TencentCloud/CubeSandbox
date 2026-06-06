// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testSandboxID = "sb-test-001"

func TestNewConfigFromEnv(t *testing.T) {
	clearEnv(t)

	cfg := NewConfigFromEnv()
	if cfg.APIURL != defaultAPIURL {
		t.Fatalf("APIURL=%q, want %q", cfg.APIURL, defaultAPIURL)
	}
	if cfg.ProxyPortHTTP != 80 {
		t.Fatalf("ProxyPortHTTP=%d, want 80", cfg.ProxyPortHTTP)
	}
	if cfg.ProxyScheme != "http" {
		t.Fatalf("ProxyScheme=%q, want http", cfg.ProxyScheme)
	}
	if cfg.SandboxDomain != "cube.app" {
		t.Fatalf("SandboxDomain=%q, want cube.app", cfg.SandboxDomain)
	}
	if cfg.TemplateID != "" || cfg.ProxyNodeIP != "" {
		t.Fatalf("unexpected default template/proxy: %#v", cfg)
	}

	t.Setenv("E2B_API_URL", "http://e2b.local:3000/")
	t.Setenv("E2B_API_KEY", "e2b-key")
	t.Setenv("CUBE_API_URL", "http://cube.local:3000/")
	t.Setenv("CUBE_API_KEY", "cube-key")
	t.Setenv("CUBE_TEMPLATE_ID", "tpl-env")
	t.Setenv("CUBE_PROXY_NODE_IP", "10.0.0.8")
	t.Setenv("CUBE_PROXY_PORT_HTTP", "9090")
	t.Setenv("CUBE_PROXY_SCHEME", "https")
	t.Setenv("CUBE_SANDBOX_DOMAIN", "sandbox.internal")
	t.Setenv("CUBE_TIMEOUT", "600")
	t.Setenv("CUBE_REQUEST_TIMEOUT", "2s")

	cfg = NewConfigFromEnv()
	if cfg.APIURL != "http://cube.local:3000" {
		t.Fatalf("APIURL=%q", cfg.APIURL)
	}
	if cfg.APIKey != "cube-key" || cfg.TemplateID != "tpl-env" {
		t.Fatalf("APIKey/TemplateID mismatch: %#v", cfg)
	}
	if cfg.ProxyNodeIP != "10.0.0.8" || cfg.ProxyPortHTTP != 9090 {
		t.Fatalf("proxy mismatch: %#v", cfg)
	}
	if cfg.ProxyScheme != "https" {
		t.Fatalf("ProxyScheme=%q", cfg.ProxyScheme)
	}
	if cfg.SandboxDomain != "sandbox.internal" {
		t.Fatalf("SandboxDomain=%q", cfg.SandboxDomain)
	}
	if cfg.Timeout != 600*time.Second || cfg.RequestTimeout != 2*time.Second {
		t.Fatalf("timeouts mismatch: %#v", cfg)
	}
}

func TestCreateSendsPythonCompatiblePayload(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("Authorization=%q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-env"))
	}))
	defer server.Close()

	disallowInternet := false
	client := NewClient(Config{
		APIURL:         server.URL + "/",
		APIKey:         "test-key",
		TemplateID:     "tpl-env",
		Timeout:        300 * time.Second,
		RequestTimeout: time.Second,
		SandboxDomain:  "cube.app",
	})

	sb, err := client.Create(context.Background(), CreateOptions{
		Timeout:             600 * time.Second,
		EnvVars:             map[string]string{"FOO": "bar"},
		Metadata:            map[string]string{"network-policy": "custom"},
		AllowInternetAccess: &disallowInternet,
		Network: NetworkOptions{
			AllowOut: []string{"8.8.8.8/32"},
			DenyOut:  []string{"0.0.0.0/0"},
		},
		Extra: map[string]any{"mcp": map[string]any{"enabled": true}},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if sb.SandboxID != testSandboxID || sb.Domain != "cube.app" {
		t.Fatalf("sandbox mismatch: %#v", sb)
	}

	assertString(t, got, "templateID", "tpl-env")
	assertNumber(t, got, "timeout", 600)
	assertMapString(t, got["envVars"], "FOO", "bar")
	assertMapString(t, got["metadata"], "network-policy", "custom")
	if got["allowInternetAccess"] != false {
		t.Fatalf("allowInternetAccess=%#v, want false", got["allowInternetAccess"])
	}
	network, ok := got["network"].(map[string]any)
	if !ok {
		t.Fatalf("network=%#v", got["network"])
	}
	assertStringSlice(t, network["allowOut"], []string{"8.8.8.8/32"})
	assertStringSlice(t, network["denyOut"], []string{"0.0.0.0/0"})
	if _, ok := got["mcp"].(map[string]any); !ok {
		t.Fatalf("extra field not preserved: %#v", got["mcp"])
	}
}

func TestCreateOmitsOptionalFieldsAndRequiresTemplate(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-explicit"))
	}))
	defer server.Close()

	allowInternet := true
	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	if _, err := client.Create(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("Create without template returned nil error")
	}

	_, err := client.Create(context.Background(), CreateOptions{
		TemplateID:          "tpl-explicit",
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, ok := got["allowInternetAccess"]; ok {
		t.Fatalf("allowInternetAccess should be omitted when true: %#v", got)
	}
	if _, ok := got["network"]; ok {
		t.Fatalf("network should be omitted when empty: %#v", got)
	}
}

func TestLifecycleEndpoints(t *testing.T) {
	var calls []string
	var connectTimeout, resumeTimeout int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/"+testSandboxID+"/connect":
			var body map[string]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode connect: %v", err)
			}
			connectTimeout = body["timeout"]
			fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-test"))
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes":
			fmt.Fprint(w, "["+sandboxInfoJSON(testSandboxID, "running")+"]")
		case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
			fmt.Fprint(w, "["+sandboxInfoJSON(testSandboxID, "paused")+"]")
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			fmt.Fprint(w, `{"status":"ok","sandboxes":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/"+testSandboxID:
			fmt.Fprint(w, sandboxInfoJSON(testSandboxID, "paused"))
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/"+testSandboxID+"/pause":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/"+testSandboxID+"/resume":
			var body map[string]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode resume: %v", err)
			}
			resumeTimeout = body["timeout"]
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, sandboxJSON(testSandboxID, "tpl-test"))
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/"+testSandboxID:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-test", Timeout: 600 * time.Second})
	ctx := context.Background()

	sb, err := client.Connect(ctx, testSandboxID)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if connectTimeout != 600 {
		t.Fatalf("connect timeout=%d", connectTimeout)
	}

	list, err := client.List(ctx)
	if err != nil || len(list) != 1 || list[0].State != "running" {
		t.Fatalf("List=%#v err=%v", list, err)
	}
	list, err = client.ListV2(ctx)
	if err != nil || len(list) != 1 || list[0].State != "paused" {
		t.Fatalf("ListV2=%#v err=%v", list, err)
	}
	health, err := client.Health(ctx)
	if err != nil || health["status"] != "ok" {
		t.Fatalf("Health=%#v err=%v", health, err)
	}
	info, err := sb.GetInfo(ctx)
	if err != nil || info.State != "paused" {
		t.Fatalf("GetInfo=%#v err=%v", info, err)
	}
	wait := false
	if err := sb.Pause(ctx, PauseOptions{Wait: &wait}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := sb.Resume(ctx, 120*time.Second); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumeTimeout != 120 {
		t.Fatalf("resume timeout=%d", resumeTimeout)
	}
	if err := sb.Kill(ctx); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	want := []string{
		"POST /sandboxes/" + testSandboxID + "/connect",
		"GET /sandboxes",
		"GET /v2/sandboxes",
		"GET /health",
		"GET /sandboxes/" + testSandboxID,
		"POST /sandboxes/" + testSandboxID + "/pause",
		"POST /sandboxes/" + testSandboxID + "/resume",
		"DELETE /sandboxes/" + testSandboxID,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestAPIErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		target     error
		call       func(*Client) error
	}{
		{
			name:       "authentication",
			statusCode: http.StatusUnauthorized,
			body:       `{"message":"bad key"}`,
			target:     ErrAuthentication,
			call: func(c *Client) error {
				_, err := c.Health(context.Background())
				return err
			},
		},
		{
			name:       "template not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"template not found"}`,
			target:     ErrTemplateNotFound,
			call: func(c *Client) error {
				_, err := c.Create(context.Background(), CreateOptions{})
				return err
			},
		},
		{
			name:       "template not found in backend 500",
			statusCode: http.StatusInternalServerError,
			body:       `{"message":"CubeMaster returned error code 130404: failed to get template param from store: template not found"}`,
			target:     ErrTemplateNotFound,
			call: func(c *Client) error {
				_, err := c.Create(context.Background(), CreateOptions{})
				return err
			},
		},
		{
			name:       "sandbox not found",
			statusCode: http.StatusNotFound,
			body:       `{"message":"sandbox not found"}`,
			target:     ErrSandboxNotFound,
			call: func(c *Client) error {
				_, err := c.Connect(context.Background(), testSandboxID)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			client := NewClient(Config{APIURL: server.URL, TemplateID: "tpl-test"})
			err := tt.call(client)
			if !errors.Is(err, tt.target) {
				t.Fatalf("errors.Is(%v, %v)=false", err, tt.target)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.statusCode {
				t.Fatalf("APIError mismatch: %#v", err)
			}
		})
	}
}

func TestParseLine(t *testing.T) {
	execution := &Execution{}
	var stdoutCalls, stderrCalls, resultCalls, errorCalls int
	opts := RunCodeOptions{
		OnStdout: func(message OutputMessage) {
			stdoutCalls++
			if message.Text != "hello\n" {
				t.Fatalf("stdout callback text=%q", message.Text)
			}
		},
		OnStderr: func(message OutputMessage) {
			stderrCalls++
			if !message.IsStderr {
				t.Fatal("stderr callback IsStderr=false")
			}
		},
		OnResult: func(result Result) {
			resultCalls++
			if result.Text != "42" {
				t.Fatalf("result callback text=%q", result.Text)
			}
		},
		OnError: func(execErr ExecutionError) {
			errorCalls++
			if execErr.Name != "ValueError" {
				t.Fatalf("error callback name=%q", execErr.Name)
			}
		},
	}

	parseLine(execution, []byte(`{"type":"stdout","text":"hello\n","timestamp":"t1"}`), opts)
	parseLine(execution, []byte(`{"type":"stderr","text":"warn\n","timestamp":"t2"}`), opts)
	parseLine(execution, []byte(`{"type":"result","text":"42","is_main_result":true}`), opts)
	parseLine(execution, []byte(`{"type":"error","name":"ValueError","value":"bad","traceback":["l1"]}`), opts)
	parseLine(execution, []byte(`{"type":"number_of_executions","execution_count":5}`), opts)
	parseLine(execution, []byte(`not json`), opts)
	parseLine(execution, []byte(`{"type":"unknown","text":"ignored"}`), opts)

	if execution.Text != "42" || execution.Logs.Stdout[0] != "hello\n" || execution.Logs.Stderr[0] != "warn\n" {
		t.Fatalf("execution mismatch: %#v", execution)
	}
	if execution.Error == nil || execution.Error.Value != "bad" {
		t.Fatalf("error mismatch: %#v", execution.Error)
	}
	if execution.ExecutionCount == nil || *execution.ExecutionCount != 5 {
		t.Fatalf("execution count mismatch: %#v", execution.ExecutionCount)
	}
	if stdoutCalls != 1 || stderrCalls != 1 || resultCalls != 1 || errorCalls != 1 {
		t.Fatalf("callback counts=%d/%d/%d/%d", stdoutCalls, stderrCalls, resultCalls, errorCalls)
	}
}

func TestParseLineAcceptsStringTraceback(t *testing.T) {
	execution := &Execution{}
	parseLine(execution, []byte(`{"type":"error","name":"ValueError","value":"bad","traceback":"trace text"}`), RunCodeOptions{})

	if execution.Error == nil {
		t.Fatal("error event was not parsed")
	}
	if len(execution.Error.Traceback) != 1 || execution.Error.Traceback[0] != "trace text" {
		t.Fatalf("traceback=%#v", execution.Error.Traceback)
	}
}

func TestRunCodeUsesProxyNodeIPAndPreservesHost(t *testing.T) {
	var gotHost string
	var gotPayload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/execute" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		gotHost = r.Host
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"type":"stdout","text":"out\n","timestamp":"t1"}`)
		fmt.Fprintln(w, `{"type":"stderr","text":"err\n","timestamp":"t2"}`)
		fmt.Fprintln(w, `{"type":"result","text":"ok","is_main_result":true}`)
		fmt.Fprintln(w, `{"type":"number_of_executions","execution_count":7}`)
		fmt.Fprintln(w, `not json`)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{
		ProxyNodeIP:    host,
		ProxyPortHTTP:  port,
		SandboxDomain:  "cube.test",
		RequestTimeout: time.Second,
		Timeout:        300 * time.Second,
	})
	sb := &Sandbox{client: client, SandboxID: "sb-proxy", TemplateID: "tpl-test"}

	var stdout []string
	execution, err := sb.RunCode(context.Background(), "1 + 1", RunCodeOptions{
		Language: "python",
		Envs:     map[string]string{"A": "B"},
		OnStdout: func(message OutputMessage) {
			stdout = append(stdout, message.Text)
		},
	})
	if err != nil {
		t.Fatalf("RunCode: %v", err)
	}

	if gotHost != "49999-sb-proxy.cube.test" {
		t.Fatalf("Host=%q", gotHost)
	}
	assertString(t, gotPayload, "code", "1 + 1")
	assertString(t, gotPayload, "language", "python")
	assertMapString(t, gotPayload["env_vars"], "A", "B")
	if execution.Text != "ok" || execution.Logs.Stderr[0] != "err\n" || *execution.ExecutionCount != 7 {
		t.Fatalf("execution=%#v", execution)
	}
	if strings.Join(stdout, "") != "out\n" {
		t.Fatalf("stdout callback=%#v", stdout)
	}
}

func TestRunCodeUsesConfiguredProxyScheme(t *testing.T) {
	var gotScheme string
	client := NewClient(Config{
		ProxyScheme: "https",
	}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotScheme = req.URL.Scheme
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})}))
	sb := &Sandbox{client: client, SandboxID: "sb-scheme", Domain: "cube.test"}

	if _, err := sb.RunCode(context.Background(), "1", RunCodeOptions{}); err != nil {
		t.Fatalf("RunCode: %v", err)
	}
	if gotScheme != "https" {
		t.Fatalf("scheme=%q", gotScheme)
	}
}

func TestCommandsRun(t *testing.T) {
	starter := &fakeProcessStarter{
		result: &processStartResult{
			Stdout:   "hello\nworld\n",
			Stderr:   "warn\n",
			ExitCode: 0,
		},
	}
	commands := &Commands{starter: starter}

	result, err := commands.Run(context.Background(), "echo hello", CommandOptions{
		Timeout: 5 * time.Second,
		Envs:    map[string]string{"A": "B"},
		Cwd:     "/work",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if starter.payload.Process.Cmd != "/bin/bash" {
		t.Fatalf("process cmd=%q", starter.payload.Process.Cmd)
	}
	if got := strings.Join(starter.payload.Process.Args, "\x00"); got != "-l\x00-c\x00echo hello" {
		t.Fatalf("process args=%#v", starter.payload.Process.Args)
	}
	if starter.payload.Process.Envs["A"] != "B" || starter.payload.Process.Cwd != "/work" {
		t.Fatalf("process env/cwd mismatch: %#v", starter.payload.Process)
	}
	if starter.payload.Stdin == nil || *starter.payload.Stdin {
		t.Fatalf("stdin=%v, want false", starter.payload.Stdin)
	}
	if starter.opts.Timeout != 5*time.Second {
		t.Fatalf("timeout=%s", starter.opts.Timeout)
	}
	if result.Stdout != "hello\nworld\n" || result.Stderr != "warn\n" || result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}

	starter = &fakeProcessStarter{
		result: &processStartResult{ExitCode: 1},
	}
	result, err = (&Commands{starter: starter}).Run(context.Background(), "false", CommandOptions{})
	if err != nil {
		t.Fatalf("Run false: %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code=%d", result.ExitCode)
	}

	starter = &fakeProcessStarter{
		result: &processStartResult{Stdout: "42\n"},
	}
	result, err = (&Commands{starter: starter}).Run(context.Background(), "echo 42", CommandOptions{})
	if err != nil {
		t.Fatalf("Run numeric stdout: %v", err)
	}
	if result.Stdout != "42\n" || result.ExitCode != 0 {
		t.Fatalf("numeric stdout result=%#v", result)
	}
}

func TestFilesRead(t *testing.T) {
	reader := &fakeFileReader{content: "file content"}
	content, err := (&Files{reader: reader}).Read(context.Background(), "/tmp/foo.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "file content" {
		t.Fatalf("content=%q", content)
	}
	if reader.path != "/tmp/foo.txt" {
		t.Fatalf("path=%q", reader.path)
	}

	reader = &fakeFileReader{}
	content, err = (&Files{reader: reader}).Read(context.Background(), "/tmp/empty.txt")
	if err != nil || content != "" {
		t.Fatalf("empty content=%q err=%v", content, err)
	}

	reader = &fakeFileReader{err: fmt.Errorf("failed to read /tmp/missing.txt: No such file")}
	_, err = (&Files{reader: reader}).Read(context.Background(), "/tmp/missing.txt")
	if err == nil || !strings.Contains(err.Error(), "failed to read /tmp/missing.txt: No such file") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestCommandsRunUsesEnvdProcessStart(t *testing.T) {
	var gotHost string
	var gotPayload map[string]any
	var gotHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/process.Process/Start" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		gotHost = r.Host
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.Header().Set("Content-Type", connectContentType)
		w.Write(connectEnvelope(0, `{"event":{"start":{"pid":123}}}`))
		w.Write(connectEnvelope(0, fmt.Sprintf(`{"event":{"data":{"stdout":%q}}}`, base64.StdEncoding.EncodeToString([]byte("cmd-out\n")))))
		w.Write(connectEnvelope(0, fmt.Sprintf(`{"event":{"data":{"stderr":%q}}}`, base64.StdEncoding.EncodeToString([]byte("cmd-err\n")))))
		w.Write(connectEnvelope(0, `{"event":{"end":{"exitCode":7,"exited":true,"status":"exited"}}}`))
		w.Write(connectEnvelope(connectEndStreamFlag, `{}`))
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
		client:          client,
		SandboxID:       "sb-proc",
		TemplateID:      "tpl-test",
		EnvdAccessToken: "envd-token",
	}

	result, err := sb.Commands().Run(context.Background(), "echo hello", CommandOptions{
		Timeout: 1500 * time.Millisecond,
		Envs:    map[string]string{"A": "B"},
		Cwd:     "/work",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotHost != "49999-sb-proc.cube.test" {
		t.Fatalf("Host=%q", gotHost)
	}
	if gotHeaders.Get("Content-Type") != connectContentType || gotHeaders.Get("Connect-Protocol-Version") != connectProtocolVersion {
		t.Fatalf("connect headers=%#v", gotHeaders)
	}
	if gotHeaders.Get("Connect-Timeout-Ms") != "1500" || gotHeaders.Get("X-Access-Token") != "envd-token" {
		t.Fatalf("headers=%#v", gotHeaders)
	}

	processPayload, ok := gotPayload["process"].(map[string]any)
	if !ok {
		t.Fatalf("process payload=%#v", gotPayload["process"])
	}
	assertString(t, processPayload, "cmd", "/bin/bash")
	assertString(t, processPayload, "cwd", "/work")
	args, ok := processPayload["args"].([]any)
	if !ok || len(args) != 3 || args[0] != "-l" || args[1] != "-c" || args[2] != "echo hello" {
		t.Fatalf("args=%#v", processPayload["args"])
	}
	assertMapString(t, processPayload["envs"], "A", "B")
	if gotPayload["stdin"] != false {
		t.Fatalf("stdin=%#v", gotPayload["stdin"])
	}
	if result.Stdout != "cmd-out\n" || result.Stderr != "cmd-err\n" || result.ExitCode != 7 {
		t.Fatalf("result=%#v", result)
	}
}

func TestFilesReadUsesEnvdHTTPFileAPI(t *testing.T) {
	var gotHost string
	var gotPath string
	var gotToken string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/files" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		gotHost = r.Host
		gotPath = r.URL.Query().Get("path")
		gotToken = r.Header.Get("X-Access-Token")
		fmt.Fprint(w, "file content")
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
		client:          client,
		SandboxID:       "sb-files",
		TemplateID:      "tpl-test",
		EnvdAccessToken: "envd-token",
	}

	content, err := sb.Files().Read(context.Background(), "/tmp/foo bar.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != "file content" {
		t.Fatalf("content=%q", content)
	}
	if gotHost != "49999-sb-files.cube.test" || gotPath != "/tmp/foo bar.txt" || gotToken != "envd-token" {
		t.Fatalf("host/path/token=%q/%q/%q", gotHost, gotPath, gotToken)
	}
}

func TestFilesReadReturnsEnvdFileError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"file not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	host, port := serverHostPort(t, server.URL)
	client := NewClient(Config{
		ProxyNodeIP:    host,
		ProxyPortHTTP:  port,
		SandboxDomain:  "cube.test",
		RequestTimeout: time.Second,
	})
	sb := &Sandbox{client: client, SandboxID: "sb-files", TemplateID: "tpl-test"}

	_, err := sb.Files().Read(context.Background(), "/tmp/missing.txt")
	if err == nil || !strings.Contains(err.Error(), "failed to read /tmp/missing.txt") || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("error=%v", err)
	}
	if errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("file read 404 should not be classified as sandbox not found: %v", err)
	}
}

type fakeRunner struct {
	code            string
	opts            RunCodeOptions
	stdoutCallbacks []string
	execution       *Execution
	err             error
}

func (r *fakeRunner) RunCode(_ context.Context, code string, opts RunCodeOptions) (*Execution, error) {
	r.code = code
	r.opts = opts
	for _, text := range r.stdoutCallbacks {
		if opts.OnStdout != nil {
			opts.OnStdout(OutputMessage{Text: text})
		}
	}
	if r.execution == nil {
		r.execution = &Execution{}
	}
	return r.execution, r.err
}

type fakeProcessStarter struct {
	payload processStartRequest
	opts    CommandOptions
	result  *processStartResult
	err     error
}

func (s *fakeProcessStarter) startProcess(_ context.Context, payload processStartRequest, opts CommandOptions) (*processStartResult, error) {
	s.payload = payload
	s.opts = opts
	if s.result == nil {
		s.result = &processStartResult{}
	}
	return s.result, s.err
}

type fakeFileReader struct {
	path    string
	content string
	err     error
}

func (r *fakeFileReader) readFile(_ context.Context, path string) (string, error) {
	r.path = path
	return r.content, r.err
}

func connectEnvelope(flags byte, payload string) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CUBE_API_URL",
		"CUBE_API_KEY",
		"CUBE_TEMPLATE_ID",
		"CUBE_PROXY_NODE_IP",
		"CUBE_PROXY_PORT_HTTP",
		"CUBE_PROXY_SCHEME",
		"CUBE_SANDBOX_DOMAIN",
		"CUBE_TIMEOUT",
		"CUBE_REQUEST_TIMEOUT",
		"E2B_API_URL",
		"E2B_API_KEY",
	} {
		t.Setenv(key, "")
	}
}

func sandboxJSON(sandboxID, templateID string) string {
	return fmt.Sprintf(`{"sandboxID":%q,"templateID":%q,"clientID":"client-1","envdVersion":"0.0.1","domain":"cube.app"}`, sandboxID, templateID)
}

func sandboxInfoJSON(sandboxID, state string) string {
	return fmt.Sprintf(`{"sandboxID":%q,"templateID":"tpl-test","clientID":"client-1","startedAt":"2026-05-14T00:00:00Z","endAt":"2026-05-14T01:00:00Z","envdVersion":"0.0.1","domain":"cube.app","cpuCount":2,"memoryMB":512,"state":%q}`, sandboxID, state)
}

func serverHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	host, portString, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portString, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func assertString(t *testing.T, values map[string]any, key, want string) {
	t.Helper()
	if values[key] != want {
		t.Fatalf("%s=%#v, want %q", key, values[key], want)
	}
}

func assertNumber(t *testing.T, values map[string]any, key string, want float64) {
	t.Helper()
	if values[key] != want {
		t.Fatalf("%s=%#v, want %v", key, values[key], want)
	}
}

func assertMapString(t *testing.T, value any, key, want string) {
	t.Helper()
	values, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value=%#v, want map", value)
	}
	if values[key] != want {
		t.Fatalf("%s=%#v, want %q", key, values[key], want)
	}
}

func assertStringSlice(t *testing.T, value any, want []string) {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value=%#v, want slice", value)
	}
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		got = append(got, item.(string))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("slice=%#v, want %#v", got, want)
	}
}

func TestBuildTemplateForwardsCreateFromImageOptions(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/templates" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"jobID":"job-1","templateID":"tpl-1","status":"accepted","phase":"","progress":0}`)
	}))
	defer server.Close()

	probePort := uint16(8080)
	cpu := uint32(2000)
	memory := uint32(2048)
	allowInternet := true
	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	_, err := client.BuildTemplate(context.Background(), BuildTemplateOptions{
		Image:               "registry.example.com/app:latest",
		WritableLayerSize:   "20Gi",
		ExposedPorts:        []uint16{8080},
		ProbePort:           &probePort,
		ProbePath:           "/health",
		CPU:                 &cpu,
		Memory:              &memory,
		Env:                 map[string]string{"A": "1"},
		AllowInternetAccess: &allowInternet,
		NetworkType:         "tap",
		Nodes:               []string{"node-a", "10.0.0.12"},
		RegistryUsername:    "pull-user",
		RegistryPassword:    "pull-pass",
		Command:             []string{"/bin/sh", "-c"},
		Args:                []string{"sleep infinity"},
		DNS:                 []string{"8.8.8.8", "1.1.1.1"},
		AllowOut:            []string{"172.67.0.0/16"},
		DenyOut:             []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("BuildTemplate returned error: %v", err)
	}

	assertString(t, got, "image", "registry.example.com/app:latest")
	assertString(t, got, "writableLayerSize", "20Gi")
	assertString(t, got, "networkType", "tap")
	assertString(t, got, "registryUsername", "pull-user")
	assertString(t, got, "registryPassword", "pull-pass")
	assertString(t, got, "probePath", "/health")
	assertNumber(t, got, "probePort", 8080)
	assertNumber(t, got, "cpu", 2000)
	assertNumber(t, got, "memory", 2048)
	assertStringSlice(t, got["nodes"], []string{"node-a", "10.0.0.12"})
	assertStringSlice(t, got["command"], []string{"/bin/sh", "-c"})
	assertStringSlice(t, got["args"], []string{"sleep infinity"})
	assertStringSlice(t, got["dns"], []string{"8.8.8.8", "1.1.1.1"})
	assertStringSlice(t, got["allowOut"], []string{"172.67.0.0/16"})
	assertStringSlice(t, got["denyOut"], []string{"10.0.0.0/8"})
	assertStringSlice(t, got["env"], []string{"A=1"})
	if got["allowInternetAccess"] != true {
		t.Fatalf("allowInternetAccess=%#v, want true", got["allowInternetAccess"])
	}
}

func TestBuildTemplateRequiresImage(t *testing.T) {
	client := NewClient(Config{APIURL: "http://example.com", Timeout: 300 * time.Second})
	if _, err := client.BuildTemplate(context.Background(), BuildTemplateOptions{}); err == nil {
		t.Fatal("BuildTemplate without image returned nil error")
	}
}

func TestGetTemplateParsesNetworkFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/templates/tpl-network" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"templateID":"tpl-network",
			"status":"READY",
			"networkType":"tap",
			"allowInternetAccess":false,
			"createRequest":{
				"network_type":"tap",
				"cubevs_context":{"allowOut":["172.67.0.0/16"],"denyOut":["10.0.0.0/8"]}
			}
		}`)
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, Timeout: 300 * time.Second})
	info, err := client.GetTemplate(context.Background(), "tpl-network")
	if err != nil {
		t.Fatalf("GetTemplate returned error: %v", err)
	}
	if info.TemplateID != "tpl-network" {
		t.Fatalf("TemplateID=%q, want tpl-network", info.TemplateID)
	}
	if info.NetworkType != "tap" {
		t.Fatalf("NetworkType=%q, want tap", info.NetworkType)
	}
	if info.AllowInternetAccess == nil || *info.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%#v, want false", info.AllowInternetAccess)
	}
}

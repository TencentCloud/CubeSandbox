// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeRunFakeServer()
	os.Exit(m.Run())
}

func fakeSpec(name string, env map[string]string) config.Server {
	return config.Server{
		Name:    name,
		Command: []string{"/bin/sh", "-c", testutil.FakeRunCmd(name)},
		Env:     env,
		Dir:     "/",
	}
}

func startGateway(t *testing.T, specs ...config.Server) (*Gateway, string) {
	t.Helper()
	t.Setenv(TokenEnv, "must-not-leak")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := Start(ctx, specs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler("tok"))
	t.Cleanup(srv.Close)
	return g, srv.URL
}

func TestAggregatesToolsAcrossServers(t *testing.T) {
	g, url := startGateway(t,
		fakeSpec("alpha", map[string]string{"FAKE_SECRET": "a-secret"}),
		fakeSpec("beta", nil))

	session := testutil.Connect(t, url+"/mcp", "tok")
	var names []string
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"alpha_crash", "alpha_env", "beta_crash", "beta_echo", "beta_env", "echo"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if g.Tools()["beta_echo"] != "beta" || g.Tools()["echo"] != "alpha" {
		t.Fatalf("conflicting tool routed incorrectly: %v", g.Tools())
	}

	if got := testutil.CallText(t, session, "echo", map[string]any{"text": "hi"}); got != "alpha:hi" {
		t.Errorf("echo = %q", got)
	}
	if got := testutil.CallText(t, session, "beta_echo", map[string]any{"text": "hi"}); got != "beta:hi" {
		t.Errorf("beta_echo = %q", got)
	}
	if got := testutil.CallText(t, session, "alpha_env", nil); got != "secret=a-secret token=false" {
		t.Errorf("alpha_env = %q; server envs must be applied and the gateway token stripped", got)
	}
}

func TestRestartsExitedServer(t *testing.T) {
	_, url := startGateway(t, fakeSpec("alpha", nil))
	session := testutil.Connect(t, url+"/mcp", "tok")

	_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "alpha_crash"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "echo", Arguments: map[string]any{"text": "back"},
		})
		if err == nil && !res.IsError {
			if tc := res.Content[0].(*mcp.TextContent); tc.Text != "alpha:back" {
				t.Fatalf("echo after restart = %q", tc.Text)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server was not restarted: res=%v err=%v", res, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRequiresBearerToken(t *testing.T) {
	_, url := startGateway(t, fakeSpec("alpha", nil))
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for name, header := range map[string]string{"missing": "", "wrong": "Bearer nope", "raw token": "tok"} {
		req, _ := http.NewRequest(http.MethodPost, url+"/mcp", body)
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: status = %d, want 401", name, resp.StatusCode)
		}
	}
}

func TestHealthDoesNotRequireToken(t *testing.T) {
	_, url := startGateway(t, fakeSpec("alpha", nil))
	resp, err := http.Get(url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Status  string   `json:"status"`
		Servers []string `json:"servers"`
		Tools   int      `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || len(body.Servers) != 1 || body.Tools != 3 {
		t.Fatalf("health = %+v", body)
	}
}

func TestEmptyTokenRejectsEverything(t *testing.T) {
	h := requireBearer("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not be reached")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestStartFailsWhenAnyServerFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Start(ctx, []config.Server{
		fakeSpec("alpha", nil),
		{Name: "broken", Command: []string{"/bin/sh", "-c", "echo boom >&2; exit 7"}, Dir: "/"},
	}, Options{})
	if err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRunsBeforeStart(t *testing.T) {
	dir := t.TempDir()
	spec := fakeSpec("alpha", nil)
	spec.Dir = dir
	spec.Install = [][]string{{"/bin/sh", "-c", "touch installed"}}
	startGateway(t, spec)
	if _, err := os.Stat(dir + "/installed"); err != nil {
		t.Fatalf("install command did not run in the server directory: %v", err)
	}
}

func TestInstallFailureIsReported(t *testing.T) {
	spec := fakeSpec("alpha", nil)
	spec.Install = [][]string{{"/bin/sh", "-c", "exit 4"}}
	_, err := Start(context.Background(), []config.Server{spec}, Options{})
	if err == nil || !strings.Contains(err.Error(), "installCmd") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedCloneLeavesNoRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "servers", "acme", "tools")
	spec := fakeSpec("github/acme/tools", nil)
	spec.Dir = dir
	spec.Repo = filepath.Join(t.TempDir(), "missing-repo")
	if _, err := Start(context.Background(), []config.Server{spec}, Options{}); err == nil || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("err = %v", err)
	}
	for _, path := range []string{dir, dir + ".partial"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s left behind after a failed clone: %v", path, err)
		}
	}

	src := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Leftovers of an interrupted clone must not block the next attempt.
	for _, path := range []string{dir, dir + ".partial"} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "stale"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec.Repo = src
	startGateway(t, spec)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("repository was not cloned into the server directory: %v", err)
	}
	if _, err := os.Stat(dir + ".partial"); !os.IsNotExist(err) {
		t.Errorf("partial clone directory left behind: %v", err)
	}
}

func TestPrefixWriter(t *testing.T) {
	var sb strings.Builder
	w := &prefixWriter{prefix: "[x] ", w: &sb}
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte("c\n"))
	if got := sb.String(); got != "[x] a\n[x] bc\n" {
		t.Fatalf("got %q", got)
	}
}

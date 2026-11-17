// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/testutil"
)

// childEnv makes a re-executed test binary behave like the mcp-gateway
// binary, so the launcher can spawn `serve` from os.Executable().
const childEnv = "CUBE_MCP_GATEWAY_TEST_MAIN"

func TestMain(m *testing.M) {
	testutil.MaybeRunFakeServer()
	if os.Getenv(childEnv) == "1" {
		os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr, DefaultPaths()))
	}
	os.Exit(m.Run())
}

func testPaths(t *testing.T) Paths {
	dir := t.TempDir()
	return Paths{
		TokenFile:   filepath.Join(dir, "etc", ".token"),
		CatalogFile: filepath.Join(dir, "catalog.json"),
		PIDFile:     filepath.Join(dir, "gateway.pid"),
		LogFile:     filepath.Join(dir, "gateway.log"),
		ServersDir:  filepath.Join(dir, "servers"),
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func fakeConfig(names ...string) string {
	m := map[string]any{}
	for _, n := range names {
		m[n] = map[string]any{"runCmd": testutil.FakeRunCmd(n), "envs": map[string]string{"FAKE_SECRET": n + "-secret"}}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func run(t *testing.T, paths Paths, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(args, &stdout, &stderr, paths)
	return code, stdout.String(), stderr.String()
}

func stopGateway(t *testing.T, paths Paths) {
	t.Cleanup(func() {
		if pid, ok := runningPID(paths.PIDFile); ok {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

func TestLaunchStartsBackgroundGateway(t *testing.T) {
	t.Setenv(childEnv, "1")
	t.Setenv("GATEWAY_ACCESS_TOKEN", "launch-token")
	paths := testPaths(t)
	stopGateway(t, paths)
	port := freePort(t)

	code, stdout, stderr := run(t, paths, "--config", fakeConfig("alpha"), "--port", strconv.Itoa(port), "--startup-timeout", "30s")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf("ready on port %d", port)) {
		t.Errorf("stdout = %q", stdout)
	}

	token, err := os.ReadFile(paths.TokenFile)
	if err != nil || string(token) != "launch-token" {
		t.Fatalf("token file = %q, %v", token, err)
	}
	if st, _ := os.Stat(paths.TokenFile); st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v", st.Mode().Perm())
	}
	pid, ok := runningPID(paths.PIDFile)
	if !ok {
		t.Fatal("gateway process is not running after the launcher returned")
	}
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	environ, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if bytes.Contains(cmdline, []byte("launch-token")) || bytes.Contains(environ, []byte("launch-token")) ||
		bytes.Contains(cmdline, []byte("alpha-secret")) {
		t.Error("token or server secrets leaked into the gateway command line or environment")
	}

	session := testutil.Connect(t, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "launch-token")
	if got := testutil.CallText(t, session, "echo", map[string]any{"text": "ping"}); got != "alpha:ping" {
		t.Errorf("echo = %q", got)
	}
	if got := testutil.CallText(t, session, "alpha_env", nil); got != "secret=alpha-secret token=false" {
		t.Errorf("alpha_env = %q", got)
	}

	code, _, stderr = run(t, paths, "--config", fakeConfig("alpha"), "--port", strconv.Itoa(freePort(t)))
	if code == 0 || !strings.Contains(stderr, "already running") {
		t.Errorf("second launch: exit %d, stderr %q", code, stderr)
	}
}

func TestLaunchGeneratesTokenWhenUnset(t *testing.T) {
	t.Setenv(childEnv, "1")
	t.Setenv("GATEWAY_ACCESS_TOKEN", "")
	paths := testPaths(t)
	stopGateway(t, paths)
	port := freePort(t)
	if code, _, stderr := run(t, paths, "--config", fakeConfig("alpha"), "--port", strconv.Itoa(port)); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	token, _ := os.ReadFile(paths.TokenFile)
	if len(token) != 64 {
		t.Fatalf("generated token = %q", token)
	}
	testutil.Connect(t, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), string(token))
}

func TestLaunchReportsServerFailure(t *testing.T) {
	t.Setenv(childEnv, "1")
	paths := testPaths(t)
	stopGateway(t, paths)
	code, _, stderr := run(t, paths, "--config", `{"broken": {"runCmd": "echo exploded >&2; exit 5"}}`,
		"--port", strconv.Itoa(freePort(t)))
	if code != 1 || !strings.Contains(stderr, `"broken"`) || !strings.Contains(stderr, "exploded") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(paths.TokenFile); !os.IsNotExist(err) {
		t.Errorf("token file must be removed after a failed launch: %v", err)
	}
	if _, ok := runningPID(paths.PIDFile); ok {
		t.Error("gateway still running after failure")
	}
}

func TestLaunchTimesOut(t *testing.T) {
	t.Setenv(childEnv, "1")
	paths := testPaths(t)
	stopGateway(t, paths)
	start := time.Now()
	code, _, stderr := run(t, paths, "--config", `{"slow": {"runCmd": "sleep 30"}}`,
		"--port", strconv.Itoa(freePort(t)), "--startup-timeout", "1s")
	if code != 1 || !strings.Contains(stderr, "timed out") || !strings.Contains(stderr, "mcp-gateway pull") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
}

func TestLaunchTimeoutStopsInstallCommands(t *testing.T) {
	t.Setenv(childEnv, "1")
	paths := testPaths(t)
	stopGateway(t, paths)
	pidFile := filepath.Join(t.TempDir(), "install.pid")
	cfg, _ := json.Marshal(map[string]any{"slow": map[string]any{
		"installCmd": fmt.Sprintf("sleep 300 & echo $! > %s; wait", pidFile),
		"runCmd":     "true",
	}})
	code, _, stderr := run(t, paths, "--config", string(cfg),
		"--port", strconv.Itoa(freePort(t)), "--startup-timeout", "1s")
	if code != 1 || !strings.Contains(stderr, "timed out") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("install command did not start: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("install process %d survived the startup timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReadyPipeIsNotInheritedByServers(t *testing.T) {
	t.Setenv(childEnv, "1")
	paths := testPaths(t)
	stopGateway(t, paths)
	marker := filepath.Join(t.TempDir(), "leaked")
	cfg, _ := json.Marshal(map[string]any{"alpha": map[string]any{
		"installCmd": fmt.Sprintf(`case "$(readlink /proc/$$/fd/3)" in pipe:*) touch %s;; esac`, marker),
		"runCmd":     testutil.FakeRunCmd("alpha"),
	}})
	if code, _, stderr := run(t, paths, "--config", string(cfg), "--port", strconv.Itoa(freePort(t))); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the readiness pipe leaked into an install command as fd 3")
	}
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestLaunchRejectsInvalidConfigWithoutSpawning(t *testing.T) {
	paths := testPaths(t)
	for cfg, want := range map[string]string{
		`not json`:          "JSON object",
		`{"unknown-x": {}}`: "unknown MCP server",
	} {
		code, _, stderr := run(t, paths, "--config", cfg)
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("config %s: exit %d, stderr %q", cfg, code, stderr)
		}
	}
	if code, _, stderr := run(t, paths); code != 1 || !strings.Contains(stderr, "--config is required") {
		t.Errorf("missing config: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(paths.LogFile); !os.IsNotExist(err) {
		t.Error("invalid config must fail before spawning the gateway")
	}
}

func TestServeForeground(t *testing.T) {
	paths := testPaths(t)
	port := freePort(t)
	t.Setenv("GATEWAY_ACCESS_TOKEN", "fg-token")
	errc := make(chan error, 1)
	go func() {
		var stderr bytes.Buffer
		errc <- fmt.Errorf("serve exited: %d %s", Main([]string{"serve", "--config", fakeConfig("alpha"),
			"--port", strconv.Itoa(port), "--pid-file", ""}, &bytes.Buffer{}, &stderr, paths), stderr.String())
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-errc:
			t.Fatal(err)
		default:
		}
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground gateway did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	session := testutil.Connect(t, fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "fg-token")
	if got := testutil.CallText(t, session, "echo", map[string]any{"text": "x"}); got != "alpha:x" {
		t.Errorf("echo = %q", got)
	}
}

func TestPullAndCatalog(t *testing.T) {
	paths := testPaths(t)
	marker := filepath.Join(t.TempDir(), "pulled")
	catalog := fmt.Sprintf(`{"local": {"command": [["true"]], "install": [["touch", %q]]}, "bare": {"command": [["true"]]}}`, marker)
	if err := os.WriteFile(paths.CatalogFile, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, paths, "pull", "local", "bare")
	if code != 0 {
		t.Fatalf("pull: exit %d, %s", code, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("pull did not run the install command")
	}
	if !strings.Contains(stdout, "bare: nothing to pull") {
		t.Errorf("stdout = %q", stdout)
	}
	if code, _, stderr := run(t, paths, "pull", "nope"); code != 1 || !strings.Contains(stderr, "unknown MCP server") {
		t.Errorf("pull unknown: exit %d, %q", code, stderr)
	}
	code, stdout, _ = run(t, paths, "catalog")
	if code != 0 || !strings.Contains(stdout, "duckduckgo") || !strings.Contains(stdout, "local") {
		t.Errorf("catalog: exit %d, %q", code, stdout)
	}
}

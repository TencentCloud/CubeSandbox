//go:build e2e

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package e2e drives the real cubeops binary against a real MySQL instance over
// HTTP. It is excluded from the default unit gate by the e2e build tag; run it
// with:
//
//	cd CubeOps && go test -tags e2e ./e2e/...
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

const (
	requireDockerEnv = "CUBEOPS_REQUIRE_DOCKER_TESTS"
	mysqlTag         = "8.0"
	dbName           = "cubeops_e2e"
)

type env struct {
	t        *testing.T
	binary   string
	dsn      string
	mysqlURL struct{ host, port string }
	logDir   string
	teardown func()
}

func requireDocker() bool {
	v := os.Getenv(requireDockerEnv)
	if v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	ci := os.Getenv("CI")
	return ci == "true" || ci == "1"
}

func abortOrSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if requireDocker() {
		t.Fatalf("%s (set %s or fix Docker — CI forbids skip)", msg, requireDockerEnv)
	}
	t.Skipf("%s", msg)
}

// newEnv builds the cubeops binary and starts a throwaway MySQL container.
func newEnv(t *testing.T) *env {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkip(t, "dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkip(t, "docker daemon not reachable (%v)", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "mysql",
		Tag:        mysqlTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=" + dbName,
		},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		abortOrSkip(t, "could not start mysql container (%v)", err)
	}

	port := resource.GetPort("3306/tcp")
	dsn := fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/%s?charset=utf8&parseTime=true", port, dbName)

	// MySQL's entrypoint runs a temporary server during initialisation and then
	// restarts, so the first successful connection can still be to the throwaway
	// instance. Require a working query, then a short settle, before proceeding.
	pool.MaxWait = 3 * time.Minute
	if err := pool.Retry(func() error {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		var one int
		return db.QueryRow("SELECT 1").Scan(&one)
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("mysql never became reachable: %v", err)
	}
	time.Sleep(2 * time.Second)

	binary := filepath.Join(t.TempDir(), "cubeops")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cubeops")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("building cubeops failed: %v\n%s", err, out)
	}

	e := &env{t: t, binary: binary, dsn: dsn, logDir: t.TempDir()}
	e.mysqlURL.host, e.mysqlURL.port = "127.0.0.1", port
	e.teardown = func() { _ = pool.Purge(resource) }
	return e
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type instance struct {
	baseURL string
	cmd     *exec.Cmd
	logPath string
}

// start launches cubeops and waits for /health. extraEnv entries are KEY=VALUE.
func (e *env) start(t *testing.T, extraEnv ...string) *instance {
	t.Helper()
	port := freePort(t)
	logPath := filepath.Join(e.logDir, fmt.Sprintf("cubeops-%d.out", port))
	out, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}

	cmd := exec.Command(e.binary)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CUBE_OPS_BIND=127.0.0.1:%d", port),
		"CUBE_SANDBOX_MYSQL_HOST="+e.mysqlURL.host,
		"CUBE_SANDBOX_MYSQL_PORT="+e.mysqlURL.port,
		"CUBE_SANDBOX_MYSQL_USER=root",
		"CUBE_SANDBOX_MYSQL_PASSWORD=root",
		"CUBE_SANDBOX_MYSQL_DB="+dbName,
		"CUBE_OPS_LOG_DIR="+filepath.Join(e.logDir, fmt.Sprintf("log-%d", port)),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cubeops: %v", err)
	}

	inst := &instance{baseURL: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, logPath: logPath}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(inst.baseURL + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return inst
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	inst.stop()
	t.Fatalf("cubeops never became healthy on %s\n--- output ---\n%s", inst.baseURL, tailLines(string(body), 25))
	return nil
}

// startExpectingExit launches cubeops and waits for it to terminate, returning
// its combined output. It fails the test if the process stays up.
func (e *env) startExpectingExit(t *testing.T, extraEnv ...string) string {
	t.Helper()
	port := freePort(t)
	cmd := exec.Command(e.binary)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CUBE_OPS_BIND=127.0.0.1:%d", port),
		"CUBE_SANDBOX_MYSQL_HOST="+e.mysqlURL.host,
		"CUBE_SANDBOX_MYSQL_PORT="+e.mysqlURL.port,
		"CUBE_SANDBOX_MYSQL_USER=root",
		"CUBE_SANDBOX_MYSQL_PASSWORD=root",
		"CUBE_SANDBOX_MYSQL_DB="+dbName,
		"CUBE_OPS_LOG_DIR="+filepath.Join(e.logDir, fmt.Sprintf("exitlog-%d", port)),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("cubeops kept running; expected it to abort\n%s", tailLines(string(out), 20))
	}
	return string(out)
}

func (i *instance) stop() {
	if i == nil || i.cmd == nil || i.cmd.Process == nil {
		return
	}
	_ = i.cmd.Process.Kill()
	_, _ = i.cmd.Process.Wait()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (e *env) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", e.dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db
}

// do issues a request and returns the status and body.
func do(t *testing.T, method, url, bearer, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// login performs a real login and returns the access token.
func login(t *testing.T, baseURL, user, pass string) string {
	t.Helper()
	code, body := do(t, http.MethodPost, baseURL+"/api/v1/auth/login", "",
		fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass))
	if code != http.StatusOK {
		t.Fatalf("login returned %d: %s", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("login response is not JSON: %s", body)
	}
	for _, key := range []string{"accessToken", "access_token"} {
		if v, ok := parsed[key].(string); ok && v != "" {
			return v
		}
	}
	t.Fatalf("no access token in login response: %s", body)
	return ""
}

// extractRefreshToken pulls the refresh token out of a login response.
func extractRefreshToken(t *testing.T, body string) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("login response is not JSON: %s", body)
	}
	for _, key := range []string{"refreshToken", "refresh_token"} {
		if v, ok := parsed[key].(string); ok && v != "" {
			return v
		}
	}
	t.Fatalf("no refresh token in login response: %s", body)
	return ""
}

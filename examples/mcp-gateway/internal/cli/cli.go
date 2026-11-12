// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the E2B-compatible mcp-gateway command line:
//
//	mcp-gateway --config '<json>'   start the gateway in the background
//	mcp-gateway serve ...           run the gateway in the foreground
//	mcp-gateway pull <server>...    pre-fetch catalog servers into the image
//	mcp-gateway catalog             list built-in servers
package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/config"
	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/gateway"
)

// Defaults match the E2B gateway contract: port 50005, token file
// /etc/mcp-gateway/.token.
const (
	DefaultPort           = 50005
	DefaultTokenFile      = "/etc/mcp-gateway/.token"
	DefaultCatalogFile    = "/etc/mcp-gateway/catalog.json"
	DefaultPIDFile        = "/run/mcp-gateway.pid"
	DefaultLogFile        = "/var/log/mcp-gateway.log"
	DefaultStartupTimeout = 55 * time.Second
	logTailBytes          = 4096
)

// Paths groups the filesystem locations used by the gateway so tests can
// relocate them.
type Paths struct {
	TokenFile   string
	CatalogFile string
	PIDFile     string
	LogFile     string
	ServersDir  string
}

// DefaultPaths returns the in-sandbox locations.
func DefaultPaths() Paths {
	return Paths{
		TokenFile:   DefaultTokenFile,
		CatalogFile: DefaultCatalogFile,
		PIDFile:     DefaultPIDFile,
		LogFile:     DefaultLogFile,
		ServersDir:  config.DefaultServersDir,
	}
}

// envelope is handed from the launcher to the background process on stdin so
// neither the config (which may carry secrets) nor the token touches disk or
// the process command line.
type envelope struct {
	Config json.RawMessage `json:"config"`
	Token  string          `json:"token"`
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer, paths Paths) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return exit(stderr, serveCmd(args[1:], stderr, paths))
		case "pull":
			return exit(stderr, pullCmd(args[1:], stdout, stderr, paths))
		case "catalog":
			return exit(stderr, catalogCmd(stdout, paths))
		case "version":
			fmt.Fprintln(stdout, gateway.Version)
			return 0
		}
	}
	return exit(stderr, launchCmd(args, stdout, stderr, paths))
}

func exit(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	fmt.Fprintln(stderr, "mcp-gateway:", err)
	return 1
}

func launchCmd(args []string, stdout, stderr io.Writer, paths Paths) error {
	fs := flag.NewFlagSet("mcp-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgJSON := fs.String("config", "", "MCP server configuration as a JSON object keyed by server name")
	cfgFile := fs.String("config-file", "", "read the MCP server configuration from a file")
	port := fs.Int("port", DefaultPort, "port to listen on")
	timeout := fs.Duration("startup-timeout", DefaultStartupTimeout, "how long to wait for every MCP server to start")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := readConfig(*cfgJSON, *cfgFile)
	if err != nil {
		return err
	}
	cat, err := config.LoadCatalog(paths.CatalogFile)
	if err != nil {
		return err
	}
	_, warnings, err := config.Resolve(raw, cat, paths.ServersDir)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if pid, ok := runningPID(paths.PIDFile); ok {
		return fmt.Errorf("gateway is already running (pid %d)", pid)
	}

	token := os.Getenv(gateway.TokenEnv)
	if token == "" {
		if token, err = randomToken(); err != nil {
			return err
		}
	}
	if err := writePrivateFile(paths.TokenFile, []byte(token)); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	pid, err := spawn(raw, token, *port, *timeout, paths)
	if err != nil {
		_ = os.Remove(paths.TokenFile)
		return err
	}
	fmt.Fprintf(stdout, "MCP gateway is ready on port %d (pid %d)\n", *port, pid)
	return nil
}

func readConfig(inline, file string) ([]byte, error) {
	switch {
	case inline != "" && file != "":
		return nil, errors.New("--config and --config-file are mutually exclusive")
	case inline != "":
		return []byte(inline), nil
	case file != "":
		return os.ReadFile(file)
	}
	return nil, errors.New("--config is required")
}

// spawn starts `mcp-gateway serve` in its own session and waits until it
// reports readiness, fails, or times out.
func spawn(raw []byte, token string, port int, timeout time.Duration, paths Paths) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogFile), 0o755); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer readyR.Close()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		readyW.Close()
		return 0, err
	}

	cmd := exec.Command(exe, "serve", "--config-stdin", "--ready-fd", "3",
		"--port", strconv.Itoa(port),
		"--pid-file", paths.PIDFile,
		"--catalog-file", paths.CatalogFile,
		"--servers-dir", paths.ServersDir)
	cmd.Env = withoutEnv(os.Environ(), gateway.TokenEnv)
	cmd.Stdin = stdinR
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	stdinR.Close()
	readyW.Close()
	if startErr != nil {
		stdinW.Close()
		return 0, startErr
	}

	msg, _ := json.Marshal(envelope{Config: raw, Token: token})
	_, writeErr := stdinW.Write(msg)
	stdinW.Close()

	result := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(readyR).ReadString('\n')
		result <- strings.TrimSpace(line)
	}()
	fail := func(reason string) (int, error) {
		stopServe(cmd)
		return 0, fmt.Errorf("%s\n%s", reason, logTail(paths.LogFile))
	}
	if writeErr != nil {
		return fail("failed to hand over configuration: " + writeErr.Error())
	}
	select {
	case line := <-result:
		switch {
		case line == "ok":
			pid := cmd.Process.Pid
			_ = cmd.Process.Release()
			return pid, nil
		case strings.HasPrefix(line, "error: "):
			return fail(strings.TrimPrefix(line, "error: "))
		default:
			return fail("gateway exited before it became ready")
		}
	case <-time.After(timeout):
		return fail(fmt.Sprintf("timed out after %s waiting for MCP servers to start; "+
			"pre-install them in the template with `mcp-gateway pull <server>`", timeout))
	}
}

// serveStopGrace bounds how long a failed serve may take to cancel its
// install commands; it has to fit in the SDK's 60s wait on top of the default
// startup timeout.
const serveStopGrace = 3 * time.Second

// stopServe asks serve to shut down with SIGTERM, which cancels install
// commands running in their own process groups, and SIGKILLs serve's process
// group if it does not exit in time.
func stopServe(cmd *exec.Cmd) {
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(serveStopGrace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-exited
}

func serveCmd(args []string, stderr io.Writer, paths Paths) error {
	fs := flag.NewFlagSet("mcp-gateway serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgJSON := fs.String("config", "", "MCP server configuration JSON")
	cfgFile := fs.String("config-file", "", "read the MCP server configuration from a file")
	cfgStdin := fs.Bool("config-stdin", false, "read the configuration and token envelope from stdin")
	port := fs.Int("port", DefaultPort, "port to listen on")
	readyFD := fs.Int("ready-fd", 0, "file descriptor to report readiness on")
	pidFile := fs.String("pid-file", paths.PIDFile, "pid file path; empty disables it")
	catalogFile := fs.String("catalog-file", paths.CatalogFile, "extra catalog JSON file")
	serversDir := fs.String("servers-dir", paths.ServersDir, "directory for cloned GitHub servers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var ready *os.File
	if *readyFD > 0 {
		// Inherited descriptors survive exec; keep MCP servers and install
		// commands from holding the pipe open or writing to it.
		syscall.CloseOnExec(*readyFD)
		ready = os.NewFile(uintptr(*readyFD), "ready")
	}
	report := func(err error) error {
		if ready != nil {
			if err != nil {
				fmt.Fprintf(ready, "error: %s\n", strings.ReplaceAll(err.Error(), "\n", "; "))
			} else {
				fmt.Fprintln(ready, "ok")
			}
			ready.Close()
			ready = nil
		}
		return err
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	raw, token, err := serveInput(*cfgStdin, *cfgJSON, *cfgFile)
	if err != nil {
		return report(err)
	}
	cat, err := config.LoadCatalog(*catalogFile)
	if err != nil {
		return report(err)
	}
	servers, warnings, err := config.Resolve(raw, cat, *serversDir)
	if err != nil {
		return report(err)
	}
	for _, w := range warnings {
		logger.Warn(w)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return report(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	g, err := gateway.Start(ctx, servers, gateway.Options{Logger: logger, Output: stderr})
	if err != nil {
		ln.Close()
		return report(err)
	}
	defer g.Close()
	if *pidFile != "" {
		if err := writePrivateFile(*pidFile, []byte(strconv.Itoa(os.Getpid()))); err != nil {
			ln.Close()
			return report(fmt.Errorf("write pid file: %w", err))
		}
		defer os.Remove(*pidFile)
	}

	srv := &http.Server{Handler: g.Handler(token), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("mcp gateway listening", "port", *port, "servers", len(servers), "tools", len(g.Tools()))
	_ = report(nil)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func serveInput(fromStdin bool, inline, file string) ([]byte, string, error) {
	if fromStdin {
		var env envelope
		if err := json.NewDecoder(os.Stdin).Decode(&env); err != nil {
			return nil, "", fmt.Errorf("read configuration from stdin: %w", err)
		}
		if env.Token == "" {
			return nil, "", errors.New("missing access token")
		}
		return env.Config, env.Token, nil
	}
	raw, err := readConfig(inline, file)
	if err != nil {
		return nil, "", err
	}
	token := os.Getenv(gateway.TokenEnv)
	if token == "" {
		return nil, "", fmt.Errorf("%s must be set", gateway.TokenEnv)
	}
	_ = os.Unsetenv(gateway.TokenEnv)
	return raw, token, nil
}

func pullCmd(args []string, stdout, stderr io.Writer, paths Paths) error {
	if len(args) == 0 {
		return errors.New("usage: mcp-gateway pull <server>...")
	}
	cat, err := config.LoadCatalog(paths.CatalogFile)
	if err != nil {
		return err
	}
	for _, name := range args {
		entry, ok := cat[name]
		if !ok {
			return fmt.Errorf("unknown MCP server %q; supported servers: %s", name, strings.Join(cat.Names(), ", "))
		}
		if len(entry.Install) == 0 {
			fmt.Fprintf(stdout, "%s: nothing to pull\n", name)
			continue
		}
		for _, argv := range entry.Install {
			fmt.Fprintf(stdout, "%s: %s\n", name, strings.Join(argv, " "))
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("pull %s: %w", name, err)
			}
		}
	}
	return nil
}

func catalogCmd(stdout io.Writer, paths Paths) error {
	cat, err := config.LoadCatalog(paths.CatalogFile)
	if err != nil {
		return err
	}
	for _, name := range cat.Names() {
		fmt.Fprintf(stdout, "%-20s %s\n", name, cat[name].Description)
	}
	return nil
}

func runningPID(pidFile string) (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !bytes.Contains(cmdline, []byte("serve")) {
		return 0, false
	}
	return pid, true
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func logTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > logTailBytes {
		_, _ = f.Seek(-logTailBytes, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(string(data))
}

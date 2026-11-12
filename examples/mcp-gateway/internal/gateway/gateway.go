// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package gateway aggregates stdio MCP servers behind one streamable-HTTP
// MCP endpoint.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/config"
)

// Version is reported in the MCP initialize handshake.
var Version = "dev"

// sessionIdleTimeout bounds how long an idle downstream MCP session is kept.
const sessionIdleTimeout = time.Hour

// TokenEnv carries the gateway bearer token. It is stripped from the
// environment of upstream servers.
const TokenEnv = "GATEWAY_ACCESS_TOKEN"

var toolNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Options configures a Gateway.
type Options struct {
	Logger *slog.Logger
	// Output receives upstream install and stderr output.
	Output io.Writer
}

// Gateway owns the upstream sessions and the aggregated MCP server.
type Gateway struct {
	server    *mcp.Server
	upstreams []*upstream
	logger    *slog.Logger

	mu    sync.Mutex
	tools map[string]string // exported tool name -> upstream server name
}

type upstream struct {
	spec   config.Server
	client *mcp.Client
	logger *slog.Logger
	output io.Writer

	mu      sync.Mutex
	session *mcp.ClientSession
}

// Start prepares and connects every server, then registers their tools. It
// fails if any server cannot be started.
func Start(ctx context.Context, servers []config.Server, opts Options) (*Gateway, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	output := opts.Output
	if output == nil {
		output = io.Discard
	}
	g := &Gateway{
		server: mcp.NewServer(&mcp.Implementation{Name: "cube-mcp-gateway", Version: Version}, &mcp.ServerOptions{
			Logger: logger,
		}),
		logger: logger,
		tools:  map[string]string{},
	}
	for _, spec := range servers {
		g.upstreams = append(g.upstreams, &upstream{
			spec: spec,
			client: mcp.NewClient(&mcp.Implementation{Name: "cube-mcp-gateway", Version: Version}, &mcp.ClientOptions{
				Logger: logger,
			}),
			logger: logger.With("server", spec.Name),
			output: output,
		})
	}

	tools := make([][]*mcp.Tool, len(g.upstreams))
	errs := make([]error, len(g.upstreams))
	var wg sync.WaitGroup
	for i, u := range g.upstreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools[i], errs[i] = u.start(ctx)
			if errs[i] != nil {
				errs[i] = fmt.Errorf("mcp server %q: %w", u.spec.Name, errs[i])
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		g.Close()
		return nil, err
	}
	for i, u := range g.upstreams {
		for _, t := range tools[i] {
			g.register(u, t)
		}
	}
	return g, nil
}

// Tools returns a copy of the exported-name to server-name mapping.
func (g *Gateway) Tools() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(g.tools))
	for k, v := range g.tools {
		out[k] = v
	}
	return out
}

// Close terminates every upstream server.
func (g *Gateway) Close() {
	for _, u := range g.upstreams {
		u.close()
	}
}

func (g *Gateway) register(u *upstream, tool *mcp.Tool) {
	original := tool.Name
	exported := original
	g.mu.Lock()
	if _, taken := g.tools[exported]; taken {
		exported = toolNameSanitizer.ReplaceAllString(u.spec.Name, "_") + "_" + original
	}
	if _, taken := g.tools[exported]; taken {
		g.mu.Unlock()
		g.logger.Warn("skipping duplicate tool", "server", u.spec.Name, "tool", original)
		return
	}
	g.tools[exported] = u.spec.Name
	g.mu.Unlock()

	t := *tool
	t.Name = exported
	if t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object"}
	}
	defer func() {
		if r := recover(); r != nil {
			g.mu.Lock()
			delete(g.tools, exported)
			g.mu.Unlock()
			g.logger.Warn("skipping tool with invalid schema", "server", u.spec.Name, "tool", original, "error", r)
		}
	}()
	g.server.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return u.call(ctx, original, req.Params)
	})
}

// Handler serves the MCP endpoint at /mcp, protected by token, and an
// unauthenticated readiness probe at /health.
func (g *Gateway) Handler(token string) http.Handler {
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return g.server }, &mcp.StreamableHTTPOptions{
		SessionTimeout: sessionIdleTimeout,
		Logger:         g.logger,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(token, stream))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		servers := make([]string, 0, len(g.upstreams))
		for _, u := range g.upstreams {
			servers = append(servers, u.spec.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"servers": servers,
			"tools":   len(g.Tools()),
		})
	})
	return mux
}

func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-gateway"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (u *upstream) start(ctx context.Context) ([]*mcp.Tool, error) {
	if err := u.prepare(ctx); err != nil {
		return nil, err
	}
	session, err := u.getSession(ctx)
	if err != nil {
		return nil, err
	}
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		tools = append(tools, tool)
	}
	u.logger.Info("mcp server ready", "tools", len(tools))
	return tools, nil
}

func (u *upstream) prepare(ctx context.Context) error {
	if u.spec.Repo != "" {
		if _, err := os.Stat(filepath.Join(u.spec.Dir, ".git")); err != nil {
			if _, err := exec.LookPath("git"); err != nil {
				return errors.New("git is required to clone GitHub MCP servers; install it in the template")
			}
			if err := os.MkdirAll(filepath.Dir(u.spec.Dir), 0o755); err != nil {
				return err
			}
			u.logger.Info("cloning repository", "repo", u.spec.Repo)
			// Clone next to Dir and rename, so an interrupted clone never leaves a
			// Dir/.git that later starts would mistake for a complete checkout.
			partial := u.spec.Dir + ".partial"
			if err := os.RemoveAll(partial); err != nil {
				return err
			}
			if err := u.run(ctx, "/", []string{"git", "clone", "--depth", "1", u.spec.Repo, partial}); err != nil {
				_ = os.RemoveAll(partial)
				return fmt.Errorf("clone %s: %w", u.spec.Repo, err)
			}
			if err := os.RemoveAll(u.spec.Dir); err != nil {
				return err
			}
			if err := os.Rename(partial, u.spec.Dir); err != nil {
				return err
			}
		}
	}
	for _, argv := range u.spec.Install {
		u.logger.Info("running install command")
		if err := u.run(ctx, u.spec.Dir, argv); err != nil {
			return fmt.Errorf("installCmd: %w", err)
		}
	}
	return nil
}

func (u *upstream) run(ctx context.Context, dir string, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = u.environ()
	cmd.Stdout = u.prefixed()
	cmd.Stderr = u.prefixed()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd.Run()
}

func (u *upstream) environ() []string {
	env := make([]string, 0, len(os.Environ())+len(u.spec.Env))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, TokenEnv+"=") {
			env = append(env, kv)
		}
	}
	for k, v := range u.spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (u *upstream) prefixed() io.Writer {
	return &prefixWriter{prefix: "[" + u.spec.Name + "] ", w: u.output}
}

// getSession returns the live session, starting the server process when it
// is not running (first use or after it exited).
func (u *upstream) getSession(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session != nil {
		return u.session, nil
	}
	cmd := exec.Command(u.spec.Command[0], u.spec.Command[1:]...)
	cmd.Dir = u.spec.Dir
	cmd.Env = u.environ()
	cmd.Stderr = u.prefixed()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	session, err := u.client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("start %q: %w", strings.Join(u.spec.Command, " "), err)
	}
	u.session = session
	go func() {
		_ = session.Wait()
		u.mu.Lock()
		if u.session == session {
			u.session = nil
			u.logger.Warn("mcp server exited; it will be restarted on the next call")
		}
		u.mu.Unlock()
	}()
	return session, nil
}

func (u *upstream) call(ctx context.Context, name string, params *mcp.CallToolParamsRaw) (*mcp.CallToolResult, error) {
	session, err := u.getSession(ctx)
	if err != nil {
		return nil, err
	}
	forward := &mcp.CallToolParams{Name: name}
	if params != nil {
		forward.Meta = params.Meta
		if len(params.Arguments) > 0 {
			forward.Arguments = params.Arguments
		}
	}
	return session.CallTool(ctx, forward)
}

func (u *upstream) close() {
	u.mu.Lock()
	session := u.session
	u.session = nil
	u.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}

type prefixWriter struct {
	mu     sync.Mutex
	prefix string
	w      io.Writer
	midLn  bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var buf []byte
	for _, c := range b {
		if !p.midLn {
			buf = append(buf, p.prefix...)
			p.midLn = true
		}
		buf = append(buf, c)
		if c == '\n' {
			p.midLn = false
		}
	}
	if _, err := p.w.Write(buf); err != nil {
		return 0, err
	}
	return len(b), nil
}

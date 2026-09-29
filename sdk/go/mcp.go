// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrMCPGateway wraps failures to start mcp-gateway during Client.Create.
var ErrMCPGateway = errors.New("cubesandbox: failed to start MCP gateway")

// MCPPort is the port the in-sandbox mcp-gateway listens on (E2B-compatible).
const MCPPort = 50005

// MCPTokenPath holds the gateway bearer token inside the sandbox.
const MCPTokenPath = "/etc/mcp-gateway/.token"

// DefaultMCPTemplateID is used by Create when MCP is set without a template.
const DefaultMCPTemplateID = "mcp-gateway"

// mcpStartupTimeout bounds `mcp-gateway --config`, matching the E2B SDK's
// default command timeout. Pre-install servers in the template.
const mcpStartupTimeout = 60 * time.Second

// MCPServers is the E2B-compatible `mcp` option: server name to per-server
// configuration, e.g. {"duckduckgo": {}, "arxiv": {"storagePath": "/"}}.
// Keys of the form "github/<owner>/<repo>" take a GitHubMCPServer.
type MCPServers map[string]any

// GitHubMCPServer configures a server cloned from github.com.
type GitHubMCPServer struct {
	// RunCmd starts a stdio MCP server in the repository root.
	RunCmd string `json:"runCmd"`
	// InstallCmd installs dependencies in the repository root.
	InstallCmd string `json:"installCmd,omitempty"`
	// Envs are set on the MCP server process.
	Envs map[string]string `json:"envs,omitempty"`
}

// GetMCPURL returns the streamable-HTTP URL of the sandbox MCP gateway. Send
// "Authorization: Bearer <token>" from GetMCPToken with every request.
func (s *Sandbox) GetMCPURL() string {
	scheme := "http"
	if s.client != nil {
		scheme = s.client.config.ProxyScheme
	}
	return scheme + "://" + s.GetHost(MCPPort) + "/mcp"
}

// GetMCPToken returns the MCP gateway bearer token, or "" if MCP is not
// enabled. The token is cached on the Sandbox that started the gateway; other
// instances (e.g. from Connect) read it from the sandbox.
func (s *Sandbox) GetMCPToken(ctx context.Context) (string, error) {
	if s.mcpToken != "" {
		return s.mcpToken, nil
	}
	files := s.Files().ForUser(defaultEnvdUser)
	ok, err := files.Exists(ctx, MCPTokenPath)
	if err != nil || !ok {
		return "", err
	}
	token, err := files.Read(ctx, MCPTokenPath)
	if err != nil {
		return "", err
	}
	s.mcpToken = strings.TrimSpace(token)
	return s.mcpToken, nil
}

func (s *Sandbox) startMCPGateway(ctx context.Context, servers MCPServers) error {
	raw, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("encode mcp config: %w", err)
	}
	token, err := newMCPToken()
	if err != nil {
		return err
	}
	result, err := s.Commands().Run(ctx, "mcp-gateway --config "+shellQuote(string(raw)), CommandOptions{
		User:    defaultEnvdUser,
		Envs:    map[string]string{"GATEWAY_ACCESS_TOKEN": token},
		Timeout: mcpStartupTimeout,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMCPGateway, err)
	}
	if result.ExitCode != 0 {
		detail := result.Stderr
		if strings.TrimSpace(detail) == "" {
			detail = result.Stdout
		}
		detail = strings.TrimSpace(detail)
		if result.ExitCode == 127 {
			detail = fmt.Sprintf("template %q does not provide mcp-gateway (see docs/guide/mcp-gateway.md): %s", s.TemplateID, detail)
		}
		return fmt.Errorf("%w: %s", ErrMCPGateway, detail)
	}
	s.mcpToken = token
	return nil
}

func newMCPToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate MCP token: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

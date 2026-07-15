// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const envdPort = 49983
const envdAuth = "Basic cm9vdDo="
const connectJSON = "application/connect+json"

// envdHTTPClient is a dedicated client for envd command execution.
// The restart script can take up to ~15s, so we allow 60s headroom.
var envdHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
}

// CommandOutput holds the result of an envd command execution.
type CommandOutput struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// runEnvdCommand executes a process command inside a sandbox via the envd Connect API.
// Matches the old Rust run_envd_command + connect_envelope + parse_connect_stream logic.
func runEnvdCommand(httpClient *http.Client, sandboxID, domain string, req map[string]interface{}) (*CommandOutput, error) {
	host := fmt.Sprintf("%d-%s.%s", envdPort, sandboxID, domain)
	proxyURL := os.Getenv("AGENTHUB_SANDBOX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1"
	}
	proxyURL = strings.TrimRight(proxyURL, "/")
	url := fmt.Sprintf("%s/process.Process/Start", proxyURL)

	// Serialize request JSON
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal envd request: %w", err)
	}

	// Wrap in Connect envelope: [0x00] [4-byte big-endian length] [payload]
	body := make([]byte, 5+len(payload))
	body[0] = 0
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)

	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// In Go's net/http, the Host header must be set via req.Host, NOT
	// req.Header.Set("Host", ...) — the latter is silently ignored.
	httpReq.Host = host
	httpReq.Header.Set("Content-Type", connectJSON)
	httpReq.Header.Set("Authorization", envdAuth)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("envd request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("envd returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read envd response: %w", err)
	}

	return parseConnectStream(respBytes)
}

// parseConnectStream parses the Connect protocol response stream.
// Each frame: [1 byte flags] [4-byte big-endian length] [JSON payload]
func parseConnectStream(data []byte) (*CommandOutput, error) {
	out := &CommandOutput{}
	i := 0

	for i+5 <= len(data) {
		flags := data[i]
		length := binary.BigEndian.Uint32(data[i+1 : i+5])
		i += 5

		if i+int(length) > len(data) {
			return nil, fmt.Errorf("truncated envd command stream")
		}

		payload := data[i : i+int(length)]
		i += int(length)

		var v map[string]interface{}
		if err := json.Unmarshal(payload, &v); err != nil {
			continue // skip invalid JSON
		}

		// Error frame (flags bit 1 set)
		if flags&0b10 != 0 {
			if _, hasError := v["error"]; hasError {
				return nil, fmt.Errorf("envd command error: %v", v)
			}
			continue
		}

		event, ok := v["event"].(map[string]interface{})
		if !ok {
			continue
		}

		// Data event: collect stdout/stderr
		if eventData, ok := event["data"].(map[string]interface{}); ok {
			if stdout, ok := eventData["stdout"].(string); ok {
				out.Stdout += decodeB64Lossy(stdout)
			}
			if stderr, ok := eventData["stderr"].(string); ok {
				out.Stderr += decodeB64Lossy(stderr)
			}
		}

		// End event: extract exit code
		if end, ok := event["end"].(map[string]interface{}); ok {
			if exitCode, ok := end["exitCode"].(float64); ok {
				out.ExitCode = int(exitCode)
			}
		}
	}

	return out, nil
}

// readOpenclawGatewayTokenFromHost reads the gateway auth token directly from
// the host-side OpenClaw state directory (shared_files persistence mode).
// This avoids a round-trip through envd and is more reliable because the host
// file is not subject to in-process rewrites by OpenClaw during startup.
// Matches old Rust read_openclaw_gateway_token_from_host.
func readOpenclawGatewayTokenFromHost(statePath string) string {
	if statePath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(statePath, "openclaw.json"))
	if err != nil {
		return ""
	}
	var v struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v.Gateway.Auth.Token)
}

// resolveGatewayToken reads the gateway token with the same priority as the
// old Rust code (CubeAPI/src/handlers/agenthub.rs):
//  1. host-side file (shared_files mode only)
//  2. sandbox-side file via envd (single read)
//  3. fallback (the token CubeOps generated and passed to the apply script)
//
// A 5-second sleep is performed first to let OpenClaw finish its in-process
// config reload after the apply script writes openclaw.json. Without this
// delay, the host/sandbox file may still contain a transient token that
// OpenClaw generates during startup, which differs from the token the apply
// script wrote. This matches the Rust `tokio::time::sleep(Duration::from_secs(5))`.
func resolveGatewayToken(httpClient *http.Client, sandboxID, domain, hostStatePath, fallbackToken string) string {
	// Wait for OpenClaw's in-process reload window to settle.
	time.Sleep(5 * time.Second)

	// 1. Try host-side first (shared_files mode).
	if hostToken := readOpenclawGatewayTokenFromHost(hostStatePath); hostToken != "" {
		slog.Info("resolveGatewayToken: using host-side token",
			"sandboxID", sandboxID, "hostStatePath", hostStatePath)
		return hostToken
	}
	// 2. Single read from sandbox-side via envd.
	if sandboxToken := readOpenclawGatewayToken(httpClient, sandboxID, domain); sandboxToken != "" {
		slog.Info("resolveGatewayToken: using sandbox-side token",
			"sandboxID", sandboxID)
		return sandboxToken
	}
	// 3. Fallback to the generated token.
	slog.Info("resolveGatewayToken: using fallback (generated) token",
		"sandboxID", sandboxID)
	return fallbackToken
}

// readOpenclawGatewayToken reads the gateway auth token from
// /root/.openclaw/openclaw.json inside the sandbox via envd.
// Matches old Rust read_openclaw_gateway_token.
func readOpenclawGatewayToken(httpClient *http.Client, sandboxID, domain string) string {
	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd": "/bin/bash",
			"args": []string{"-l", "-c", `python3 - <<'PY'
import json
try:
    token = json.load(open('/root/.openclaw/openclaw.json')).get('gateway', {}).get('auth', {}).get('token')
    if token:
        print(token)
except Exception:
    pass
PY`},
			"envs": map[string]string{},
			"cwd":  "/root",
		},
		"stdin": false,
	}

	output, err := runEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil || output.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(output.Stdout)
}

func decodeB64Lossy(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	return string(decoded)
}

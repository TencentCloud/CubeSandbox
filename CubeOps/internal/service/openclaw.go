// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package service contains the business logic for CubeOps, decoupled from the
// HTTP (gin) layer. Handlers in package handler are thin adapters that decode
// requests, call service methods, and serialise the results. All OpenClaw
// runtime orchestration, envd command execution, host-side state management,
// and LLM config resolution live here so they can be unit-tested without
// spinning up an HTTP server.
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// ── envd command execution ──────────────────────────────────────────────────

const (
	EnvdPort       = 49983
	envdAuth       = "Basic cm9vdDo="
	connectJSON    = "application/connect+json"
	OpenclawUIPort = 18789
)

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

// EnvdHTTPClient returns the shared http.Client used for envd calls.
// Exposed so handlers (and tests) can substitute a custom client if needed.
func EnvdHTTPClient() *http.Client { return envdHTTPClient }

// RunEnvdCommand executes a process command inside a sandbox via the envd
// Connect API. Matches the old Rust run_envd_command + connect_envelope +
// parse_connect_stream logic.
func RunEnvdCommand(httpClient *http.Client, sandboxID, domain string, req map[string]interface{}) (*CommandOutput, error) {
	host := fmt.Sprintf("%d-%s.%s", EnvdPort, sandboxID, domain)
	proxyURL := os.Getenv("AGENTHUB_SANDBOX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1"
	}
	proxyURL = strings.TrimRight(proxyURL, "/")
	requestURL := fmt.Sprintf("%s/process.Process/Start", proxyURL)

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal envd request: %w", err)
	}

	// Wrap in Connect envelope: [0x00] [4-byte big-endian length] [payload]
	body := make([]byte, 5+len(payload))
	body[0] = 0
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)

	httpReq, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
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

func decodeB64Lossy(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s
	}
	return string(decoded)
}

// ── OpenClaw gateway token resolution ───────────────────────────────────────

// ReadOpenclawGatewayTokenFromHost reads the gateway auth token directly from
// the host-side OpenClaw state directory (shared_files persistence mode).
// This avoids a round-trip through envd and is more reliable because the host
// file is not subject to in-process rewrites by OpenClaw during startup.
func ReadOpenclawGatewayTokenFromHost(statePath string) string {
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

// ResolveGatewayToken reads the gateway token with the same priority as the
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
func ResolveGatewayToken(httpClient *http.Client, sandboxID, domain, hostStatePath, fallbackToken string) string {
	time.Sleep(5 * time.Second)

	if hostToken := ReadOpenclawGatewayTokenFromHost(hostStatePath); hostToken != "" {
		slog.Info("ResolveGatewayToken: using host-side token",
			"sandboxID", sandboxID, "hostStatePath", hostStatePath)
		return hostToken
	}
	if sandboxToken := readOpenclawGatewayToken(httpClient, sandboxID, domain); sandboxToken != "" {
		slog.Info("ResolveGatewayToken: using sandbox-side token",
			"sandboxID", sandboxID)
		return sandboxToken
	}
	slog.Info("ResolveGatewayToken: using fallback (generated) token",
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

	output, err := RunEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil || output.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(output.Stdout)
}

// ── OpenClaw restart / upgrade scripts ──────────────────────────────────────

// openclawRestartScript is the bash script that restarts the OpenClaw gateway
// process inside a sandbox via envd. Identical to the old Rust implementation.
const openclawRestartScript = `set -e
kill_openclaw_listeners() {
  python3 - <<'PY'
import os, pathlib, signal, time
port = int(os.environ.get("OPENCLAW_PORT", "18789"))
port_hex = f"{port:04X}"
inodes = set()
for name in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        for line in pathlib.Path(name).read_text().splitlines()[1:]:
            cols = line.split()
            if cols[1].rsplit(":", 1)[-1].upper() == port_hex and cols[3] == "0A":
                inodes.add(cols[9])
    except Exception:
        pass
pids = set()
for pid in filter(str.isdigit, os.listdir("/proc")):
    fd_dir = f"/proc/{pid}/fd"
    try:
        for fd in os.listdir(fd_dir):
            try:
                target = os.readlink(f"{fd_dir}/{fd}")
            except Exception:
                continue
            if target.startswith("socket:[") and target[8:-1] in inodes:
                pids.add(int(pid))
    except Exception:
        pass
for sig in (signal.SIGTERM, signal.SIGKILL):
    for pid in sorted(pids):
        if pid == os.getpid():
            continue
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass
        except Exception:
            pass
    time.sleep(0.5)
PY
}
restart_openclaw_service() {
  if [ -n "${OPENCLAW_NODE_EXTRA_CA_CERTS:-}" ] && [ -f "${OPENCLAW_NODE_EXTRA_CA_CERTS}" ]; then
    export NODE_EXTRA_CA_CERTS="${OPENCLAW_NODE_EXTRA_CA_CERTS}"
  elif [ -f "/root/.openclaw/cube-egress-ca.crt" ]; then
    export NODE_EXTRA_CA_CERTS="/root/.openclaw/cube-egress-ca.crt"
  fi
  if command -v supervisorctl >/dev/null 2>&1; then
    supervisorctl restart openclaw
  else
    pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
    pkill -f 'node .*openclaw' 2>/dev/null || true
    kill_openclaw_listeners
    mkdir -p /var/log
    if command -v openclaw >/dev/null 2>&1; then
      nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
    elif [ -x /opt/openclaw/openclaw ]; then
      nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
    elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
      (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
      (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /opt/openclaw/package.json ] && command -v pnpm >/dev/null 2>&1; then
      (cd /opt/openclaw && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
    elif [ -f /app/package.json ] && command -v pnpm >/dev/null 2>&1; then
      (cd /app && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
    else
      echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
      return 127
    fi
  fi
}
openclaw_ready() {
  python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
}
restart_openclaw_service
for i in $(seq 1 30); do
  if openclaw_ready; then
    if command -v supervisorctl >/dev/null 2>&1; then
      supervisorctl status openclaw
    elif command -v ps >/dev/null 2>&1; then
      ps -ef | grep -E '[o]penclaw|node .*openclaw' || true
    fi
    exit 0
  fi
  sleep 0.5
done
[ -f /var/log/openclaw.log ] && tail -80 /var/log/openclaw.log >&2 || true
exit 1`

// openclawUpgradeScript is the bash script that upgrades and restarts the
// OpenClaw gateway inside a sandbox via envd. Identical to old Rust
// upgrade_agent_openclaw.
const openclawUpgradeScript = `set -e
upgraded=0
openclaw_bin="$(command -v openclaw || true)"

if command -v npm >/dev/null 2>&1; then
  npm_json="$(npm ls -g --depth=0 --json 2>/dev/null || true)"
  npm_packages="$(printf '%s' "$npm_json" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    data = {}
for name in (data.get("dependencies") or {}):
    if "openclaw" in name.lower():
        print(name)
' || true)"
  if [ -n "$npm_packages" ]; then
    for pkg in $npm_packages; do
      npm install -g "${pkg}@latest"
      upgraded=1
    done
  fi
fi

if [ "$upgraded" != "1" ] && command -v pnpm >/dev/null 2>&1; then
  pnpm_root="$(pnpm root -g 2>/dev/null || true)"
  if [ -n "$pnpm_root" ]; then
    for pkg_dir in "$pnpm_root"/*openclaw* "$pnpm_root"/@*/*openclaw*; do
      [ -e "$pkg_dir/package.json" ] || continue
      pkg="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("name",""))' "$pkg_dir/package.json")"
      [ -n "$pkg" ] || continue
      pnpm add -g "${pkg}@latest"
      upgraded=1
    done
  fi
fi

if [ "$upgraded" != "1" ]; then
  if python3 -m pip show openclaw >/dev/null 2>&1; then
    python3 -m pip install -U openclaw
    upgraded=1
  elif command -v pip3 >/dev/null 2>&1 && pip3 show openclaw >/dev/null 2>&1; then
    pip3 install -U openclaw
    upgraded=1
  elif command -v pip >/dev/null 2>&1 && pip show openclaw >/dev/null 2>&1; then
    pip install -U openclaw
    upgraded=1
  elif command -v uv >/dev/null 2>&1 && uv pip show openclaw >/dev/null 2>&1; then
    uv pip install -U openclaw
    upgraded=1
  fi
fi

if [ "$upgraded" != "1" ]; then
  echo "OpenClaw upgrade source was not detected; refreshing existing OpenClaw service." >&2
fi
if command -v supervisorctl >/dev/null 2>&1; then
  supervisorctl restart openclaw
else
  pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
  pkill -f 'node .*openclaw' 2>/dev/null || true
  mkdir -p /var/log
  if command -v openclaw >/dev/null 2>&1; then
    nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
  elif [ -x /opt/openclaw/openclaw ]; then
    nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
  elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
    (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
  elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
    (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
  else
    echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
    exit 127
  fi
fi
for i in $(seq 1 30); do
  if python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
  then
    if command -v supervisorctl >/dev/null 2>&1; then supervisorctl status openclaw; else ps -ef | grep -E '[o]penclaw|node .*openclaw' || true; fi
    break
  fi
  sleep 0.5
done
[ -n "$openclaw_bin" ] && "$openclaw_bin" --version || true`

// RestartOpenclawForInstance restarts the OpenClaw gateway process inside the
// sandbox for the given agent instance. Returns the command output and error.
// Matches old Rust restart_openclaw_for_record.
func RestartOpenclawForInstance(inst *store.AgentInstance) (*CommandOutput, error) {
	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", openclawRestartScript},
			"envs": map[string]string{
				"NODE_EXTRA_CA_CERTS":          "/root/.openclaw/cube-egress-ca.crt",
				"OPENCLAW_NODE_EXTRA_CA_CERTS": "/root/.openclaw/cube-egress-ca.crt",
			},
			"cwd": "/root",
		},
		"stdin": false,
	}
	return RunEnvdCommand(envdHTTPClient, inst.SandboxID, inst.Domain, req)
}

// UpgradeOpenclawForInstance upgrades and restarts the OpenClaw gateway
// inside the sandbox for the given agent instance.
// Matches old Rust upgrade_agent_openclaw.
func UpgradeOpenclawForInstance(inst *store.AgentInstance) (*CommandOutput, error) {
	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", openclawUpgradeScript},
			"envs": map[string]string{
				"NODE_EXTRA_CA_CERTS":          "/root/.openclaw/cube-egress-ca.crt",
				"OPENCLAW_NODE_EXTRA_CA_CERTS": "/root/.openclaw/cube-egress-ca.crt",
			},
			"cwd": "/root",
		},
		"stdin": false,
	}
	return RunEnvdCommand(envdHTTPClient, inst.SandboxID, inst.Domain, req)
}

// ── LLM config resolution ───────────────────────────────────────────────────

const (
	openclawEgressManagedKey = "CUBE_EGRESS_MANAGED"
	defaultLLMProvider       = "deepseek"
	defaultLLMBaseURL        = "https://api.deepseek.com"
	defaultLLMCredentialMode = "egress"
	defaultOpenclawModel     = "deepseek/deepseek-v4-flash"
)

// LLMConfig holds the persisted LLM configuration from settings.
type LLMConfig struct {
	Provider       string
	BaseURL        string
	Model          string
	APIKey         string
	CredentialMode string
}

func (c *LLMConfig) UsesEgressCredentials() bool {
	return c.CredentialMode == "egress"
}

func (c *LLMConfig) OpenclawAPIKey() string {
	if c.UsesEgressCredentials() {
		return openclawEgressManagedKey
	}
	return c.APIKey
}

// LLMRuntimePlan is the fully resolved LLM config for a single sandbox.
type LLMRuntimePlan struct {
	PublicModel       string
	UpstreamModelID   string
	UpstreamProvider  string
	UpstreamBaseURL   string
	OpenclawPrimary   string
	OpenclawModelName string
	OpenclawAPIKey    string
	CredentialMode    string
}

// ResolveRuntimePlan builds the runtime plan from the persisted LLM config and
// an optional per-request model override.
func ResolveRuntimePlan(llm *LLMConfig, publicModel string) *LLMRuntimePlan {
	pm := strings.TrimSpace(publicModel)
	if pm == "" {
		pm = defaultOpenclawModel
	}
	upstreamModelID := openclawModelSuffix(pm)
	return &LLMRuntimePlan{
		PublicModel:       pm,
		UpstreamModelID:   upstreamModelID,
		UpstreamProvider:  llm.Provider,
		UpstreamBaseURL:   llm.BaseURL,
		OpenclawPrimary:   fmt.Sprintf("%s/%s", llm.Provider, upstreamModelID),
		OpenclawModelName: modelDisplayName(pm),
		OpenclawAPIKey:    llm.OpenclawAPIKey(),
		CredentialMode:    llm.CredentialMode,
	}
}

func openclawModelSuffix(model string) string {
	if idx := strings.Index(model, "/"); idx >= 0 {
		rest := model[idx+1:]
		if rest != "" {
			return rest
		}
	}
	return model
}

// extractHostFromURL returns the hostname portion of a URL, or "" on error.
func extractHostFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func modelDisplayName(model string) string {
	switch model {
	case "deepseek/deepseek-v4-pro":
		return "DeepSeek V4 Pro"
	case "deepseek/deepseek-v4-flash":
		return "DeepSeek V4 Flash"
	case "deepseek-chat":
		return "DeepSeek Chat"
	default:
		if parts := strings.Split(model, "/"); len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
		return model
	}
}

func normalizeLLMProvider(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return defaultLLMProvider
	}
	return v
}

func normalizeLLMBaseURL(raw string) string {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		return defaultLLMBaseURL
	}
	return v
}

func normalizeLLMModel(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultOpenclawModel
	}
	return v
}

func normalizeLLMCredentialMode(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "env", "environment", "legacy":
		return "env"
	default:
		return defaultLLMCredentialMode
	}
}

// DecryptSetting returns the plaintext value. If the stored value has the
// enc:v1: prefix, it decrypts it; otherwise it returns the value as-is
// (for backward compatibility with old CubeAPI plaintext storage).
func DecryptSetting(stored string) string {
	if stored == "" {
		return ""
	}
	if !strings.HasPrefix(stored, "enc:v1:") {
		return stored // plaintext (old CubeAPI format)
	}
	plain, err := crypto.DecryptSecret(stored)
	if err != nil {
		return stored // fallback to raw value if decrypt fails
	}
	return plain
}

// MaskSecret masks a secret string for safe display, keeping the first 4 and
// last 4 characters and replacing the middle with "****".
func MaskSecret(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// DefaultLLMConfig returns an LLMConfig populated with the built-in defaults.
// Used when the LLM settings cannot be resolved (e.g. API key not yet
// configured) but the caller still needs a plan to apply WeCom config.
func DefaultLLMConfig() *LLMConfig {
	return &LLMConfig{
		Provider:       defaultLLMProvider,
		BaseURL:        defaultLLMBaseURL,
		Model:          defaultOpenclawModel,
		APIKey:         "",
		CredentialMode: defaultLLMCredentialMode,
	}
}

// Exported copies of the default LLM constants so handlers (and other
// packages) can use them without depending on the resolver internals.
const (
	DefaultLLMProviderStr       = defaultLLMProvider
	DefaultLLMBaseURLStr        = defaultLLMBaseURL
	DefaultLLMModelStr          = defaultOpenclawModel
	DefaultLLMCredentialModeStr = defaultLLMCredentialMode
)

// SettingStore is the subset of *store.Store that ResolveLLMConfig needs.
// Defined as an interface so both *store.Store and any AgentStore fake
// satisfy it.
type SettingStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// ResolveLLMConfig reads LLM settings from the store
// (matching old Rust resolve_llm_config).
func ResolveLLMConfig(ctx context.Context, s SettingStore) (*LLMConfig, error) {
	provider, _ := s.GetSetting(ctx, "llm_provider")
	provider = normalizeLLMProvider(provider)

	baseURL, _ := s.GetSetting(ctx, "llm_base_url")
	baseURL = normalizeLLMBaseURL(baseURL)

	model, _ := s.GetSetting(ctx, "llm_model")
	model = normalizeLLMModel(model)

	credentialMode, _ := s.GetSetting(ctx, "llm_credential_mode")
	credentialMode = normalizeLLMCredentialMode(credentialMode)

	// Read API key (try llm_api_key first, then deepseek_api_key).
	// Matches old CubeAPI resolve_llm_config.
	apiKey, _ := s.GetSetting(ctx, "llm_api_key")
	if apiKey == "" {
		apiKey, _ = s.GetSetting(ctx, "deepseek_api_key")
	}
	apiKey = DecryptSetting(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("LLM API key is not configured. Configure it on the AgentHub settings page first")
	}

	return &LLMConfig{
		Provider:       provider,
		BaseURL:        baseURL,
		Model:          model,
		APIKey:         apiKey,
		CredentialMode: credentialMode,
	}, nil
}

// ── OpenClaw apply (writing runtime config into a sandbox) ──────────────────

// OpenclawApplyMode determines whether to do full init or just merge LLM config.
type OpenclawApplyMode int

const (
	ApplyModeFullInit OpenclawApplyMode = iota
	ApplyModeMergeLLM
)

// OpenclawApplyOptions controls how the OpenClaw runtime config is applied.
type OpenclawApplyOptions struct {
	Mode                 OpenclawApplyMode
	GatewayToken         string
	PreserveGatewayToken bool
	ConfigureWecom       bool
	BotID                string
	BotSecret            string
}

// OpenclawApplySpec renders the JSON spec handed to the sandbox apply script.
func OpenclawApplySpec(plan *LLMRuntimePlan, opts *OpenclawApplyOptions) map[string]interface{} {
	// Defensive: every production caller supplies non-nil opts, but tests and
	// future refactors might not. Default to merge_llm (the safe no-op mode)
	// rather than panicking on a nil deref.
	mode := ApplyModeMergeLLM
	preserveToken := true
	token := ""
	configureWecom := false
	var botID, botSecret string
	if opts != nil {
		mode = opts.Mode
		preserveToken = opts.PreserveGatewayToken
		token = opts.GatewayToken
		configureWecom = opts.ConfigureWecom
		botID = opts.BotID
		botSecret = opts.BotSecret
	}
	modeStr := "merge_llm"
	if mode == ApplyModeFullInit {
		modeStr = "full_init"
	}
	gatewaySpec := map[string]interface{}{
		"manage":           mode == ApplyModeFullInit,
		"preserveExisting": preserveToken,
	}
	// Only include token in spec if it's non-empty (avoids null values in JSON)
	if token != "" {
		gatewaySpec["token"] = token
	}
	spec := map[string]interface{}{
		"mode":            modeStr,
		"provider":        plan.UpstreamProvider,
		"baseUrl":         plan.UpstreamBaseURL,
		"apiKey":          plan.OpenclawAPIKey,
		"openclawPrimary": plan.OpenclawPrimary,
		"upstreamModelId": plan.UpstreamModelID,
		"modelName":       plan.OpenclawModelName,
		"credentialMode":  plan.CredentialMode,
		"configureWecom":  configureWecom,
		"gateway":         gatewaySpec,
	}
	_ = botID
	_ = botSecret
	// Resolve LLM host IP on the host side and pass via spec.
	// Egress mode blocks UDP DNS inside the sandbox; pinning the IP in
	// /etc/hosts lets OpenClaw reach the API without DNS.
	if plan.UpstreamBaseURL != "" {
		if host := extractHostFromURL(plan.UpstreamBaseURL); host != "" {
			if ips, err := net.LookupHost(host); err == nil && len(ips) > 0 {
				spec["llmHostIp"] = ips[0]
				slog.Info("resolved LLM host IP for /etc/hosts", "host", host, "ip", ips[0])
			} else {
				slog.Warn("failed to resolve LLM host IP", "host", host, "err", err)
			}
		}
	}
	return spec
}

func egressCAPem() string {
	data, _ := os.ReadFile("/etc/cube/ca/cube-root-ca.crt")
	return string(data)
}

// ApplyOpenclawRuntime writes the OpenClaw runtime config into a sandbox via envd.
// Matches old Rust apply_openclaw_runtime.
//
// The store parameter was historically present but unused inside this
// function; it has been dropped so the signature matches the applyFn field
// on AgentHubService (which needs to be injectable for tests).
func ApplyOpenclawRuntime(httpClient *http.Client, sandboxID, domain string, plan *LLMRuntimePlan, opts *OpenclawApplyOptions) (*CommandOutput, error) {
	spec := OpenclawApplySpec(plan, opts)
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal apply spec: %w", err)
	}
	specB64 := base64.StdEncoding.EncodeToString(specBytes)

	envs := map[string]string{
		"OPENCLAW_APPLY_SPEC":          specB64,
		"OPENCLAW_ALLOWED_ORIGINS":     "*",
		"CUBE_EGRESS_CA_PEM":           egressCAPem(),
		"NODE_EXTRA_CA_CERTS":          "/root/.openclaw/cube-egress-ca.crt",
		"OPENCLAW_NODE_EXTRA_CA_CERTS": "/root/.openclaw/cube-egress-ca.crt",
		"CUBE_SANDBOX_NODE_IP":         os.Getenv("CUBE_SANDBOX_NODE_IP"),
	}
	// WeCom envs are only present when the caller explicitly asked for them.
	// Guard against a nil opts (defensive — production callers always set it).
	if opts != nil && opts.ConfigureWecom {
		if opts.BotID != "" {
			envs["OPENCLAW_BOT_ID"] = opts.BotID
		}
		if opts.BotSecret != "" {
			envs["OPENCLAW_BOT_SECRET"] = opts.BotSecret
		}
	}

	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", openclawApplyScript()},
			"envs": envs,
			"cwd":  "/root",
		},
		"stdin": false,
	}

	output, err := RunEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil {
		return nil, fmt.Errorf("envd request failed: %w", err)
	}

	// Retry on config conflict (matching old Rust)
	for i := 0; i < 2; i++ {
		if output.ExitCode == 0 || !isOpenclawConfigConflict(output) {
			break
		}
		output, err = RunEnvdCommand(httpClient, sandboxID, domain, req)
		if err != nil {
			return nil, fmt.Errorf("envd retry failed: %w", err)
		}
	}

	if output.ExitCode != 0 {
		errMsg := output.Stderr
		if errMsg == "" && output.Stdout != "" {
			errMsg = "stdout: " + output.Stdout
		}
		return output, fmt.Errorf("OpenClaw runtime apply failed with exit code %d: %s", output.ExitCode, errMsg)
	}

	return output, nil
}

func isOpenclawConfigConflict(output *CommandOutput) bool {
	return strings.Contains(output.Stdout, "ConfigMutationConflictError") ||
		strings.Contains(output.Stderr, "ConfigMutationConflictError") ||
		strings.Contains(output.Stdout, "Config overwrite:") ||
		strings.Contains(output.Stderr, "Config overwrite:")
}

// LLMEgressRule builds the egress rule that injects the LLM API key into
// requests to the LLM provider's base URL. Matches old Rust llm_egress_rule.
func LLMEgressRule(llm *LLMConfig) (map[string]interface{}, error) {
	parsed, err := url.Parse(llm.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid LLM Base URL '%s': %w", llm.BaseURL, err)
	}
	scheme := parsed.Scheme
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("LLM Base URL must use http or https")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("LLM Base URL must include a host")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	path := "/*"
	if basePath != "" {
		path = basePath + "/*"
	}

	var sni *string
	if scheme == "https" {
		sni = &host
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	audit := "metadata"
	format := "Bearer ${SECRET}"

	return map[string]interface{}{
		"name": fmt.Sprintf("agenthub-llm-%s", llm.Provider),
		"match": map[string]interface{}{
			"sni":    sni,
			"host":   host,
			"method": methods,
			"path":   path,
			"scheme": scheme,
		},
		"action": map[string]interface{}{
			"allow": true,
			"audit": audit,
			"inject": []map[string]interface{}{
				{
					"header": "Authorization",
					"secret": llm.APIKey,
					"format": format,
				},
			},
		},
	}, nil
}

// AgenthubNetworkConfig builds the cube_network_config for sandbox creation.
// In egress credential mode, includes the LLM egress rule with API key injection.
// Matches old Rust agenthub_network_config.
func AgenthubNetworkConfig(llm *LLMConfig) (map[string]interface{}, error) {
	if !llm.UsesEgressCredentials() {
		return nil, nil
	}
	rule, err := LLMEgressRule(llm)
	if err != nil {
		return nil, err
	}
	allowPublicTraffic := true
	allowInternetAccess := true
	return map[string]interface{}{
		"allowInternetAccess": allowInternetAccess,
		"allowPublicTraffic":  allowPublicTraffic,
		"rules":               []map[string]interface{}{rule},
	}, nil
}

// openclawApplyScript returns the bash script that writes OpenClaw config
// inside the sandbox. Matches old Rust openclaw_apply_script() exactly.
func openclawApplyScript() string {
	return `kill_openclaw_listeners() {
           python3 - <<'PY'
import os, pathlib, signal, time
port = int(os.environ.get("OPENCLAW_PORT", "18789"))
port_hex = f"{port:04X}"
inodes = set()
for name in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        for line in pathlib.Path(name).read_text().splitlines()[1:]:
            cols = line.split()
            if cols[1].rsplit(":", 1)[-1].upper() == port_hex and cols[3] == "0A":
                inodes.add(cols[9])
    except Exception:
        pass
pids = set()
for pid in filter(str.isdigit, os.listdir("/proc")):
    fd_dir = f"/proc/{pid}/fd"
    try:
        for fd in os.listdir(fd_dir):
            try:
                target = os.readlink(f"{fd_dir}/{fd}")
            except Exception:
                continue
            if target.startswith("socket:[") and target[8:-1] in inodes:
                pids.add(int(pid))
    except Exception:
        pass
for sig in (signal.SIGTERM, signal.SIGKILL):
    for pid in sorted(pids):
        if pid == os.getpid():
            continue
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            pass
        except Exception:
            pass
    time.sleep(0.5)
PY
         }
         restart_openclaw_service() {
           kill_openclaw_listeners || true
           if command -v supervisorctl >/dev/null 2>&1; then
             supervisorctl reread || true
             supervisorctl update openclaw || true
             (supervisorctl restart openclaw || supervisorctl start openclaw) || return $?
           else
             pkill -f '(^|[ /])openclaw([ ]|$)' 2>/dev/null || true
             pkill -f 'node .*openclaw' 2>/dev/null || true
             mkdir -p /var/log
             if command -v openclaw >/dev/null 2>&1; then
               nohup openclaw gateway run >/var/log/openclaw.log 2>&1 &
             elif [ -x /opt/openclaw/openclaw ]; then
               nohup /opt/openclaw/openclaw gateway run >/var/log/openclaw.log 2>&1 &
             elif [ -f /opt/openclaw/package.json ] && command -v npm >/dev/null 2>&1; then
               (cd /opt/openclaw && nohup npm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /app/package.json ] && command -v npm >/dev/null 2>&1; then
               (cd /app && nohup npm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /opt/openclaw/package.json ] && command -v pnpm >/dev/null 2>&1; then
               (cd /opt/openclaw && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
             elif [ -f /app/package.json ] && command -v pnpm >/dev/null 2>&1; then
               (cd /app && nohup pnpm start >/var/log/openclaw.log 2>&1 &)
             else
               echo "Neither supervisorctl nor a direct OpenClaw startup command was found" >&2
               return 127
             fi
           fi
         }
         openclaw_ready() {
           python3 - <<'PY'
import json, os, socket, sys
try:
    token = json.load(open("/root/.openclaw/openclaw.json")).get("gateway", {}).get("auth", {}).get("token", "")
    port = int(os.environ.get("OPENCLAW_PORT", "18789"))
    if not token:
        sys.exit(1)
    s = socket.create_connection(("127.0.0.1", port), timeout=0.5)
    s.close()
except Exception:
    sys.exit(1)
PY
         }
         openclaw_status() {
           if command -v supervisorctl >/dev/null 2>&1; then
             supervisorctl status openclaw || true
           else
             ps -ef | grep -E '[o]penclaw|node .*openclaw' || true
             [ -f /var/log/openclaw.log ] && tail -40 /var/log/openclaw.log || true
           fi
         }
         install_wecom_plugin_if_needed() {
           if [ -n "${OPENCLAW_BOT_ID:-}" ] && [ -n "${OPENCLAW_BOT_SECRET:-}" ]; then
             if command -v openclaw >/dev/null 2>&1; then
               export NODE_EXTRA_CA_CERTS="${NODE_EXTRA_CA_CERTS:-/root/.openclaw/cube-egress-ca.crt}"
               openclaw plugins inspect wecom-openclaw-plugin >/dev/null 2>&1 || \
                 openclaw plugins install @wecom/wecom-openclaw-plugin@2026.5.7
             fi
           fi
        }
        (command -v supervisorctl >/dev/null 2>&1 && supervisorctl stop openclaw || true) && \
         install_wecom_plugin_if_needed && \
         cat >/tmp/agenthub-openclaw-apply.py <<'PY'
import base64, json, os, secrets
from datetime import datetime, timezone
from pathlib import Path

spec = json.loads(base64.b64decode(os.environ["OPENCLAW_APPLY_SPEC"]))
mode = spec["mode"]
provider = spec["provider"]
base_url = spec["baseUrl"].strip().rstrip("/")
api_key = spec["apiKey"]
credential_mode = spec.get("credentialMode", "egress")
openclaw_primary = spec["openclawPrimary"]
model_id = spec["upstreamModelId"]
model_name = spec["modelName"]
configure_wecom = bool(spec.get("configureWecom"))
gateway_spec = spec.get("gateway", {})
auth_profile = f"{provider}:default"
# For egress credential mode, use managed placeholder; otherwise use real key
auth_key = "CUBE_EGRESS_MANAGED" if credential_mode == "egress" else api_key

config_path = Path("/root/.openclaw/openclaw.json")
agent_dir = Path("/root/.openclaw/agents/main/agent")
workspace = Path("/root/.openclaw/workspace")
sessions = Path("/root/.openclaw/agents/main/sessions")
config_path.parent.mkdir(parents=True, exist_ok=True)
agent_dir.mkdir(parents=True, exist_ok=True)

ca_pem = os.environ.get("CUBE_EGRESS_CA_PEM", "").strip()
ca_path = Path(os.environ.get("OPENCLAW_NODE_EXTRA_CA_CERTS", "/root/.openclaw/cube-egress-ca.crt"))
if ca_pem:
    ca_path.parent.mkdir(parents=True, exist_ok=True)
    ca_path.write_text(ca_pem + ("\n" if not ca_pem.endswith("\n") else ""))
    os.environ["NODE_EXTRA_CA_CERTS"] = str(ca_path)

try:
    data = json.loads(config_path.read_text())
except Exception:
    data = {}
if not isinstance(data, dict):
    data = {}

# LLM blocks are written identically in both modes. Rebuilding models from
# scratch drops stale provider namespaces left by earlier configurations.
data["models"] = {
    "mode": "merge",
    "providers": {
        provider: {
            "baseUrl": base_url,
            "api": "openai-completions",
            "models": [{
                "id": model_id,
                "name": model_name,
                "reasoning": True,
                "input": ["text"],
                "contextWindow": 1000000,
                "maxTokens": 384000,
                "compat": {
                    "supportsReasoningEffort": True,
                    "supportsUsageInStreaming": True,
                    "maxTokensField": "max_tokens",
                },
                "api": "openai-completions",
            }],
        }
    },
}

agents = data.setdefault("agents", {}).setdefault("defaults", {})
agents["model"] = {"primary": openclaw_primary}
agents["models"] = {openclaw_primary: {"alias": model_name}}

plugins = data.setdefault("plugins", {}).setdefault("entries", {})
# A provider is not a plugin. Older builds registered the provider name here,
# which OpenClaw reports as "plugin not found"; drop that stale entry.
plugins.pop(provider, None)
data["auth"] = {"profiles": {auth_profile: {"provider": provider, "mode": "api_key"}}}

if mode == "full_init":
    workspace.mkdir(parents=True, exist_ok=True)
    sessions.mkdir(parents=True, exist_ok=True)
    agents["workspace"] = str(workspace)
    if gateway_spec.get("manage"):
        gateway = data.setdefault("gateway", {})
        existing = gateway.get("auth", {}).get("token", "") or ""
        token = (gateway_spec.get("token") or "").strip()
        if not token and gateway_spec.get("preserveExisting") and existing:
            token = existing
        if not token:
            token = secrets.token_hex(16)
        gateway["bind"] = "lan"
        gateway["port"] = int(os.environ.get("OPENCLAW_PORT", "18789"))
        gateway["mode"] = "local"
        gateway["tailscale"] = {"mode": "off", "resetOnExit": False}
        gateway["auth"] = {"mode": "token", "token": token}
        trusted_proxies = [
            "169.254.68.5",
            "169.254.68.0/24",
            os.environ.get("CUBE_SANDBOX_NODE_IP", "").strip(),
            "127.0.0.1",
            "::1",
        ]
        gateway["trustedProxies"] = [v for v in trusted_proxies if v]
        origins = os.environ.get("OPENCLAW_ALLOWED_ORIGINS", "*")
        gateway["controlUi"] = {
            "allowedOrigins": [o.strip() for o in origins.split(",") if o.strip()],
            "dangerouslyDisableDeviceAuth": os.environ.get("OPENCLAW_DISABLE_DEVICE_AUTH", "true").lower() == "true",
            "allowInsecureAuth": os.environ.get("OPENCLAW_ALLOW_INSECURE_AUTH", "true").lower() == "true",
            "dangerouslyAllowHostHeaderOriginFallback": os.environ.get("OPENCLAW_ALLOW_HOST_HEADER_ORIGIN_FALLBACK", "true").lower() == "true",
        }
        token_file = Path(os.environ.get("OPENCLAW_TOKEN_FILE", "/var/log/openclaw.token"))
        token_file.parent.mkdir(parents=True, exist_ok=True)
        token_file.write_text(token + "\n")
    data["session"] = {"dmScope": "per-channel-peer"}
    tools = data.setdefault("tools", {})
    tools["profile"] = "full"
    data["skills"] = {"install": {"nodeManager": "npm"}}
    data["meta"] = {
        "lastTouchedVersion": data.get("meta", {}).get("lastTouchedVersion", "2026.5.7"),
        "lastTouchedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    if configure_wecom:
        plugins["wecom-openclaw-plugin"] = {"enabled": True}
        tools["alsoAllow"] = sorted(set(tools.get("alsoAllow", []) + ["wecom_mcp"]))
        channels = data.setdefault("channels", {})
        channels["wecom"] = {
            "enabled": True,
            "connectionMode": "websocket",
            "botId": os.environ["OPENCLAW_BOT_ID"],
            "secret": os.environ["OPENCLAW_BOT_SECRET"],
            "name": "企业微信",
        }
        # Keep a small AgentHub-owned copy so the backend can return/edit the
        # binding without parsing plugin-specific channel config.
        wecom_path = config_path.parent / "agenthub-wecom.json"
        wecom_path.write_text(json.dumps({
            "botId": os.environ["OPENCLAW_BOT_ID"],
            "secret": os.environ["OPENCLAW_BOT_SECRET"],
            "enabled": True,
        }, ensure_ascii=False, indent=2) + "\n")

# Cube-proxy dials the sandbox tap IP, so merge_llm / template fast paths must
# still expose the gateway on non-loopback interfaces ("lan", not loopback/auto).
data.setdefault("gateway", {})["bind"] = "lan"

tmp = config_path.with_suffix(".json.tmp")
tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")
tmp.replace(config_path)

(agent_dir / "auth-profiles.json").write_text(json.dumps({
    "version": 1,
    "profiles": {
        auth_profile: {
            "type": "api_key",
            "provider": provider,
            "key": auth_key,
        }
    },
}, ensure_ascii=False, indent=2) + "\n")
(agent_dir / "models.json").write_text(json.dumps(data["models"], ensure_ascii=False, indent=2) + "\n")

supervisor_conf = Path("/opt/gem/supervisord/openclaw.conf")
if supervisor_conf.exists():
    lines = supervisor_conf.read_text().splitlines()
    ca_env = f',NODE_EXTRA_CA_CERTS="{ca_path}"' if ca_pem else ""
    env_line = f'environment=NODE_ENV="production",OPENCLAW_DEFAULT_MODEL="{openclaw_primary}",OPENCLAW_BIND="lan"{ca_env}'
    for idx, line in enumerate(lines):
        if line.startswith("environment="):
            lines[idx] = env_line
            break
    else:
        lines.append(env_line)
    supervisor_conf.write_text("\n".join(lines) + "\n")

print("Applied ~/.openclaw/openclaw.json")
PY
         python3 /tmp/agenthub-openclaw-apply.py && \
         restart_openclaw_service && \
         for i in $(seq 1 30); do \
           if openclaw_ready; then \
             openclaw_status; \
             break; \
           fi; \
           sleep 0.5; \
         done && \
         openclaw_ready`
}

// ── OpenClaw host-side state directory management ───────────────────────────

const (
	// Host directories for OpenClaw shared-files persistence.
	// Must be under CubeMaster's allowed_host_mount_prefixes (default: /data/shared/).
	openclawHostStateRoot    = "/data/shared/agenthub/openclaw"
	openclawHostSnapshotRoot = "/data/shared/agenthub/openclaw-snapshots"
	openclawSandboxStatePath = "/root/.openclaw"

	// HostdirMountKey is the label key under which host-mount metadata is
	// stored in CubeMaster sandbox annotations.
	// Matches old Rust HOSTDIR_MOUNT_KEY.
	HostdirMountKey = "host-mount"
)

// NewOpenclawPersistID generates a new persist ID (UUID without hyphens).
// Matches old Rust new_openclaw_persist_id.
func NewOpenclawPersistID() string {
	return uuid.New().String()
}

// GenerateGatewayToken generates a new gateway token
// (matching old Rust new_gateway_token).
func GenerateGatewayToken() string {
	return uuid.New().String()
}

// OpenclawHostStatePath returns the host path for an active OpenClaw state directory.
// Matches old Rust openclaw_host_state_path.
func OpenclawHostStatePath(persistID string) string {
	return filepath.Join(openclawHostStateRoot, persistID)
}

// OpenclawHostSnapshotPath returns the host path for a snapshot OpenClaw state directory.
// Matches old Rust openclaw_host_snapshot_path.
func OpenclawHostSnapshotPath(snapshotID string) string {
	return filepath.Join(openclawHostSnapshotRoot, snapshotID)
}

// PrepareOpenclawStateDir creates the host directory for an OpenClaw state.
// Matches old Rust prepare_openclaw_state_dir.
func PrepareOpenclawStateDir(persistID string) (string, error) {
	path := OpenclawHostStatePath(persistID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("failed to create OpenClaw state directory %s: %w", path, err)
	}
	return path, nil
}

// CopyOpenclawStateDir copies the contents of source dir to target dir using rsync.
// Matches old Rust copy_openclaw_state_dir_blocking.
// If source is empty or doesn't exist, it's a no-op.
func CopyOpenclawStateDir(source, target string) error {
	if source == "" {
		return nil
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("failed to create target OpenClaw state directory %s: %w", target, err)
	}
	cmd := exec.Command("rsync", "-a", "--delete", source+"/", target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync OpenClaw state %s -> %s failed: %w: %s", source, target, err, string(output))
	}
	return nil
}

// OpenclawHostMountMetadata builds the JSON metadata for a host directory mount.
// Matches old Rust openclaw_host_mount_metadata.
// Returns a JSON array: [{"hostPath": "...", "mountPath": "/root/.openclaw"}]
func OpenclawHostMountMetadata(hostPath string) (string, error) {
	mounts := []map[string]string{
		{"hostPath": hostPath, "mountPath": openclawSandboxStatePath},
	}
	data, err := json.Marshal(mounts)
	if err != nil {
		return "", fmt.Errorf("failed to encode OpenClaw host mount metadata: %w", err)
	}
	return string(data), nil
}

// AgenthubDistributionScope returns the distribution scope for a sandbox.
// For shared_files mode or template source, restricts to the current node.
// Matches old Rust agenthub_create_distribution_scope + agenthub_distribution_scope.
func AgenthubDistributionScope(persistenceMode, rootfsSourceType string) []string {
	// Snapshot source with non-shared-files mode → no restriction (can be on any node)
	if rootfsSourceType == "snapshot" && persistenceMode != "shared_files" {
		return nil
	}
	// Otherwise, restrict to current node (host mount is node-local)
	nodeID := os.Getenv("AGENTHUB_HOST_MOUNT_NODE_ID")
	if nodeID == "" {
		nodeID = os.Getenv("CUBE_SANDBOX_NODE_IP")
	}
	if nodeID == "" {
		return nil
	}
	return []string{nodeID}
}

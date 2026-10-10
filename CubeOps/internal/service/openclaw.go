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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
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

// readOpenclawGatewayTokenFromHostFile reads the gateway token from a named
// file in the host-side OpenClaw state dir (shared_files). Note the host file
// is the same mounted sandbox file, so it is subject to OpenClaw rewrites.
func readOpenclawGatewayTokenFromHostFile(statePath, filename string) string {
	if statePath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(statePath, filename))
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

// writeOpenclawGatewayTokenToHostFile sets gateway.auth.token in the host-side
// openclaw.json, preserving other fields. The replace is atomic so a crash
// mid-write cannot truncate the config.
func writeOpenclawGatewayTokenToHostFile(statePath, token string) error {
	path := filepath.Join(statePath, "openclaw.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}

	gateway, _ := cfg["gateway"].(map[string]interface{})
	if gateway == nil {
		gateway = map[string]interface{}{}
		cfg["gateway"] = gateway
	}
	auth, _ := gateway["auth"].(map[string]interface{})
	if auth == nil {
		auth = map[string]interface{}{}
		gateway["auth"] = auth
	}
	if _, ok := auth["mode"]; !ok {
		auth["mode"] = "token"
	}
	auth["token"] = token

	updated, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(statePath, "openclaw.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadOpenclawGatewayTokenFromHost reads the gateway token from the host-side
// openclaw.json (shared_files), avoiding an envd round-trip.
func ReadOpenclawGatewayTokenFromHost(statePath string) string {
	return readOpenclawGatewayTokenFromHostFile(statePath, "openclaw.json")
}

// readOpenclawGatewayTokenSandboxFile reads the gateway token from a named file
// in the sandbox via envd. Returns "" if the read fails or the token is absent.
func readOpenclawGatewayTokenSandboxFile(httpClient *http.Client, sandboxID, domain, filename string) string {
	if httpClient == nil {
		return ""
	}
	script := fmt.Sprintf(`python3 - <<'PY'
import json
try:
    token = json.load(open('/root/.openclaw/%s')).get('gateway', {}).get('auth', {}).get('token')
    if token:
        print(token)
except Exception:
    pass
PY`, filename)
	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", script},
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

// Gateway token resolution tunables. Vars, not consts, so tests can shrink them.
var (
	gatewayReadyTimeout  = 30 * time.Second
	gatewayReadyInterval = 500 * time.Millisecond
)

// defaultGatewayProbePath is the Control-UI bootstrap config, behind gateway
// auth: 200 for an accepted token, 401 for a rejected one. An OpenClaw-internal
// route, hence overridable in case a future release renames it.
const defaultGatewayProbePath = "/__openclaw/control-ui-config.json"

func gatewayProbePath() string {
	if path := os.Getenv("AGENTHUB_GATEWAY_PROBE_PATH"); path != "" {
		return path
	}
	return defaultGatewayProbePath
}

// gatewayAuthMode is how the live gateway answers an anonymous request.
type gatewayAuthMode int

const (
	// gatewayAuthUnknown: the probe never reached a Control-UI gateway (envd
	// failure, a dead sandbox, or an image predating the endpoint).
	gatewayAuthUnknown gatewayAuthMode = iota
	// gatewayAuthEnforced: anonymous requests are rejected, so tokens can be verified.
	gatewayAuthEnforced
	// gatewayAuthOpen: anonymous requests are served, so no token can be verified.
	gatewayAuthOpen
)

// probeGatewayAuth returns the HTTP status the live gateway answers token with.
// The bool is false when no verdict was reached; treating that as "rejected"
// would turn an envd hiccup into an auth failure.
//
// The token travels in the environment, not argv, keeping it out of the guest
// process list and envd logs.
func probeGatewayAuth(httpClient *http.Client, sandboxID, domain, token string) (int, bool) {
	if httpClient == nil {
		return 0, false
	}
	// Port read in the guest (like openclaw_ready), so CubeOps' own environment
	// cannot mislead the probe. Fixed loopback URL: no SSRF surface. ProxyHandler({})
	// forces a direct connection so the image's http_proxy cannot answer here.
	script := fmt.Sprintf(`python3 - <<'PY'
import os, urllib.error, urllib.request
port = os.environ.get("OPENCLAW_PORT", "") or "%d"
req = urllib.request.Request(f"http://127.0.0.1:{port}%s")
token = os.environ.get("OPENCLAW_PROBE_TOKEN", "")
if token:
    req.add_header("Authorization", "Bearer " + token)
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
try:
    with opener.open(req, timeout=3) as rsp:
        print(rsp.status)
except urllib.error.HTTPError as err:
    print(err.code)
except Exception:
    print(0)
PY`, OpenclawUIPort, gatewayProbePath())

	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", script},
			"envs": map[string]string{"OPENCLAW_PROBE_TOKEN": token},
			"cwd":  "/root",
		},
		"stdin": false,
	}
	output, err := RunEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil || output.ExitCode != 0 {
		return 0, false
	}
	status, err := strconv.Atoi(strings.TrimSpace(output.Stdout))
	if err != nil || status == 0 {
		return 0, false
	}
	return status, true
}

// waitGatewayAuthMode polls until the gateway answers an anonymous probe, the
// point where token verification becomes meaningful. The last status is
// returned too, telling a stale probe path (404) from a dead gateway (0).
func waitGatewayAuthMode(ctx context.Context, httpClient *http.Client, sandboxID, domain string) (gatewayAuthMode, int) {
	if httpClient == nil {
		return gatewayAuthUnknown, 0
	}
	deadline := time.Now().Add(gatewayReadyTimeout)
	lastStatus := 0
	for {
		if status, ok := probeGatewayAuth(httpClient, sandboxID, domain, ""); ok {
			lastStatus = status
			switch {
			case status == http.StatusUnauthorized:
				// 401 alone proves the gateway arbitrates on the token. A 403
				// may come from a layer no token can satisfy, so it does not
				// make tokens verifiable.
				return gatewayAuthEnforced, status
			case status == http.StatusOK:
				return gatewayAuthOpen, status
			case status >= 400 && status < 500:
				// A settled answer that simply is not about auth; retrying it
				// would only burn the timeout.
				return gatewayAuthUnknown, status
			}
		}
		if !time.Now().Before(deadline) {
			return gatewayAuthUnknown, lastStatus
		}
		select {
		case <-ctx.Done():
			return gatewayAuthUnknown, lastStatus
		case <-time.After(gatewayReadyInterval):
		}
	}
}

// Candidate token sources, ordered as gatewayTokenCandidates emits them.
const (
	gatewayTokenSourceApply    = "apply-script"
	gatewayTokenSourceConfig   = "openclaw.json"
	gatewayTokenSourceLastGood = "openclaw.json.last-good"
)

// gatewayTokenCandidate pairs a token with its origin, so the winning source
// reaches the logs and later 401 reports can be traced back.
type gatewayTokenCandidate struct {
	source string
	token  string
}

// gatewayTokenCandidates collects every plausible token, deduplicated. None is
// authoritative, hence candidates to verify rather than answers to trust.
func gatewayTokenCandidates(httpClient *http.Client, sandboxID, domain, hostStatePath, fallbackToken string) []gatewayTokenCandidate {
	read := func(filename string) string {
		if hostStatePath != "" {
			return readOpenclawGatewayTokenFromHostFile(hostStatePath, filename)
		}
		return readOpenclawGatewayTokenSandboxFile(httpClient, sandboxID, domain, filename)
	}

	ordered := []gatewayTokenCandidate{
		{gatewayTokenSourceApply, fallbackToken},
		{gatewayTokenSourceConfig, read("openclaw.json")},
		{gatewayTokenSourceLastGood, read("openclaw.json.last-good")},
	}

	candidates := make([]gatewayTokenCandidate, 0, len(ordered))
	seen := make(map[string]struct{}, len(ordered))
	for _, c := range ordered {
		if c.token == "" {
			continue
		}
		if _, dup := seen[c.token]; dup {
			continue
		}
		seen[c.token] = struct{}{}
		candidates = append(candidates, c)
	}
	return candidates
}

// unverifiedGatewayToken picks a token when the gateway cannot be probed. It
// keeps the historical preference (what OpenClaw last wrote beats what CubeOps
// asked for) so unverifiable images behave exactly as before.
func unverifiedGatewayToken(candidates []gatewayTokenCandidate) (string, string) {
	for _, source := range []string{gatewayTokenSourceConfig, gatewayTokenSourceLastGood, gatewayTokenSourceApply} {
		for _, c := range candidates {
			if c.source == source {
				return c.token, c.source
			}
		}
	}
	return "", ""
}

// ErrGatewayTokenRejected means the gateway turned down every candidate. Only
// such a confirmed rejection justifies tearing the sandbox down.
var ErrGatewayTokenRejected = errors.New("gateway rejected every candidate token")

// waitLastGoodFresherThanConfig blocks until .last-good is newer than
// openclaw.json (OpenClaw's signal that its config patch is done and the
// on-disk token is stable). Timeouts and stat errors fall through silently.
func waitLastGoodFresherThanConfig(ctx context.Context, hostStatePath string) {
	jsonPath := filepath.Join(hostStatePath, "openclaw.json")
	lgPath := filepath.Join(hostStatePath, "openclaw.json.last-good")
	deadline := time.Now().Add(gatewayReadyTimeout)
	for {
		jsonInfo, err1 := os.Stat(jsonPath)
		lgInfo, err2 := os.Stat(lgPath)
		if err1 == nil && err2 == nil && lgInfo.ModTime().After(jsonInfo.ModTime()) {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gatewayReadyInterval):
		}
	}
}

// ResolveGatewayToken resolves the gateway token to persist.
//
// Config files cannot answer this: a template image may rewrite openclaw.json
// after the gateway loaded its token, without restarting it. So we ask the
// gateway itself, and read the file only when it cannot be probed.
func ResolveGatewayToken(ctx context.Context, httpClient *http.Client, sandboxID, domain, hostStatePath, fallbackToken string) (string, error) {
	// Wait for OpenClaw to finish its own config patch before reading
	// candidates: a fresh .last-good signals the on-disk token is stable.
	if hostStatePath != "" {
		waitLastGoodFresherThanConfig(ctx, hostStatePath)
	}

	mode, lastStatus := waitGatewayAuthMode(ctx, httpClient, sandboxID, domain)

	// Read after the wait: openclaw.json is the very file OpenClaw may rewrite
	// while starting, so an earlier snapshot could already be stale.
	candidates := gatewayTokenCandidates(httpClient, sandboxID, domain, hostStatePath, fallbackToken)
	if len(candidates) == 0 {
		if mode == gatewayAuthEnforced {
			// An enforcing gateway with no token to offer can never be reached.
			logging.G(ctx).Errorf("ResolveGatewayToken: gateway enforces auth but no token exists: sandboxID=%q", sandboxID)
			return "", ErrGatewayTokenRejected
		}
		// Open or unprobeable: a token-less URL is the historical behaviour.
		logging.G(ctx).Warnf("ResolveGatewayToken: no token found, persisting a token-less URL: sandboxID=%q", sandboxID)
		return "", nil
	}

	switch mode {
	case gatewayAuthEnforced:
		rejected := 0
		for _, c := range candidates {
			status, ok := probeGatewayAuth(httpClient, sandboxID, domain, c.token)
			switch {
			case !ok:
				logging.G(ctx).Warnf("ResolveGatewayToken: probe inconclusive: sandboxID=%q source=%q", sandboxID, c.source)
			case status == http.StatusOK:
				logging.G(ctx).Infof("ResolveGatewayToken: gateway accepted token: sandboxID=%q source=%q", sandboxID, c.source)
				return c.token, nil
			case status == http.StatusUnauthorized:
				// The only status that means "this token is wrong". Anything
				// else (403 from a second authz layer, 5xx, a moved endpoint)
				// says nothing about the token and must stay inconclusive.
				rejected++
				logging.G(ctx).Warnf("ResolveGatewayToken: gateway rejected token: sandboxID=%q source=%q", sandboxID, c.source)
			default:
				logging.G(ctx).Warnf("ResolveGatewayToken: probe inconclusive: sandboxID=%q source=%q status=%d", sandboxID, c.source, status)
			}
		}
		if rejected == len(candidates) {
			logging.G(ctx).Errorf("ResolveGatewayToken: no candidate accepted: sandboxID=%q candidates=%d", sandboxID, rejected)
			return "", ErrGatewayTokenRejected
		}
		// No verdict on some candidate, so nothing is proven wrong: an envd
		// hiccup must not be mistaken for an auth failure.
		token, source := unverifiedGatewayToken(candidates)
		logging.G(ctx).Errorf("ResolveGatewayToken: probes were inconclusive, persisting unverified token: sandboxID=%q source=%q candidates=%d", sandboxID, source, len(candidates))
		return token, nil

	case gatewayAuthOpen:
		// Auth is off, so any value would let the UI in and none can be checked.
		token, source := unverifiedGatewayToken(candidates)
		logging.G(ctx).Warnf("ResolveGatewayToken: gateway serves anonymous requests, persisting unverified token: sandboxID=%q source=%q", sandboxID, source)
		return token, nil

	default:
		// Logged at error level: the token is a guess, and a 404 here means the
		// probe path is stale rather than the image being old.
		token, source := unverifiedGatewayToken(candidates)
		logging.G(ctx).Errorf("ResolveGatewayToken: gateway not probeable, persisting unverified token: sandboxID=%q source=%q lastStatus=%d probePath=%q", sandboxID, source, lastStatus, gatewayProbePath())
		return token, nil
	}
}

// SyncGatewayTokenConfig rewrites openclaw.json to advertise the token the
// gateway enforces, so the next restart cannot invalidate the URL just handed
// to the user. Reads and writes stay on the same side. Best-effort: a verified
// token is already persisted.
func SyncGatewayTokenConfig(ctx context.Context, httpClient *http.Client, sandboxID, domain, hostStatePath, enforcedToken string) {
	if enforcedToken == "" {
		return
	}

	if hostStatePath != "" {
		if readOpenclawGatewayTokenFromHostFile(hostStatePath, "openclaw.json") == enforcedToken {
			return
		}
		logging.G(ctx).Warnf("SyncGatewayTokenConfig: openclaw.json disagrees with the gateway, rewriting: sandboxID=%q", sandboxID)
		if err := writeOpenclawGatewayTokenToHostFile(hostStatePath, enforcedToken); err != nil {
			logging.G(ctx).Warnf("SyncGatewayTokenConfig: host rewrite failed: sandboxID=%q err=%q", sandboxID, err.Error())
		}
		return
	}

	if httpClient == nil {
		return
	}
	if readOpenclawGatewayTokenSandboxFile(httpClient, sandboxID, domain, "openclaw.json") == enforcedToken {
		return
	}
	logging.G(ctx).Warnf("SyncGatewayTokenConfig: openclaw.json disagrees with the gateway, rewriting: sandboxID=%q", sandboxID)

	// Token via environment, not argv; atomic replace so a crash mid-write
	// cannot leave OpenClaw with a truncated config.
	const script = `python3 - <<'PY'
import json, os, tempfile
path = "/root/.openclaw/openclaw.json"
token = os.environ.get("OPENCLAW_SYNC_TOKEN", "")
if not token:
    raise SystemExit(1)
with open(path) as fh:
    cfg = json.load(fh)
if not isinstance(cfg, dict):
    raise SystemExit(1)
auth = cfg.setdefault("gateway", {}).setdefault("auth", {})
auth.setdefault("mode", "token")
auth["token"] = token
fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
with os.fdopen(fd, "w") as fh:
    json.dump(cfg, fh, indent=2)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY`

	req := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", script},
			"envs": map[string]string{"OPENCLAW_SYNC_TOKEN": enforcedToken},
			"cwd":  "/root",
		},
		"stdin": false,
	}
	output, err := RunEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil {
		logging.G(ctx).Warnf("SyncGatewayTokenConfig: envd request failed: sandboxID=%q err=%q", sandboxID, err.Error())
		return
	}
	if output.ExitCode != 0 {
		logging.G(ctx).Warnf("SyncGatewayTokenConfig: rewrite failed: sandboxID=%q exitCode=%d stderr=%q", sandboxID, output.ExitCode, output.Stderr)
	}
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
func OpenclawApplySpec(ctx context.Context, plan *LLMRuntimePlan, opts *OpenclawApplyOptions) map[string]interface{} {
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
				logging.G(ctx).Debugf("resolved LLM host IP for /etc/hosts: host=%s ip=%s", host, ips[0])
			} else {
				logging.G(ctx).Warnf("failed to resolve LLM host IP: host=%s err=%q", host, err.Error())
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
func ApplyOpenclawRuntime(ctx context.Context, httpClient *http.Client, sandboxID, domain string, plan *LLMRuntimePlan, opts *OpenclawApplyOptions) (*CommandOutput, error) {
	spec := OpenclawApplySpec(ctx, plan, opts)
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
					// Plaintext API key injected as the egress Authorization header.
					// redact.Value() masks it by name at the log call site.
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
         sleep 2 && \
         for i in $(seq 1 60); do \
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
	openclawHostSnapshotRoot = "/data/shared/agenthub/openclaw-snapshots"
	openclawSandboxStatePath = "/root/.openclaw"

	// HostdirMountKey is the label key under which host-mount metadata is
	// stored in CubeMaster sandbox annotations.
	// Matches old Rust HOSTDIR_MOUNT_KEY.
	HostdirMountKey = "host-mount"
)

// openclawHostStateRoot is the parent of every active shared-files state dir.
// A var, not a const, so tests can redirect it to a writable temp root.
var openclawHostStateRoot = "/data/shared/agenthub/openclaw"

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

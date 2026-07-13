// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// generateGatewayToken generates a new gateway token (matching old Rust new_gateway_token).
func generateGatewayToken() string {
	return uuid.New().String()
}

const (
	openclawEgressManagedKey = "CUBE_EGRESS_MANAGED"
	defaultLLMProvider       = "deepseek"
	defaultLLMBaseURL        = "https://api.deepseek.com"
	defaultLLMCredentialMode = "egress"
	defaultOpenclawModel     = "deepseek/deepseek-v4-flash"
)

// llmConfig holds the persisted LLM configuration from settings.
type llmConfig struct {
	Provider       string
	BaseURL        string
	Model          string
	APIKey         string
	CredentialMode string
}

func (c *llmConfig) usesEgressCredentials() bool {
	return c.CredentialMode == "egress"
}

func (c *llmConfig) openclawAPIKey() string {
	if c.usesEgressCredentials() {
		return openclawEgressManagedKey
	}
	return c.APIKey
}

// llmRuntimePlan is the fully resolved LLM config for a single sandbox.
type llmRuntimePlan struct {
	PublicModel       string
	UpstreamModelID   string
	UpstreamProvider  string
	UpstreamBaseURL   string
	OpenclawPrimary   string
	OpenclawModelName string
	OpenclawAPIKey    string
	CredentialMode    string
}

func resolveRuntimePlan(llm *llmConfig, publicModel string) *llmRuntimePlan {
	pm := strings.TrimSpace(publicModel)
	if pm == "" {
		pm = defaultOpenclawModel
	}
	upstreamModelID := openclawModelSuffix(pm)
	return &llmRuntimePlan{
		PublicModel:       pm,
		UpstreamModelID:   upstreamModelID,
		UpstreamProvider:  llm.Provider,
		UpstreamBaseURL:   llm.BaseURL,
		OpenclawPrimary:   fmt.Sprintf("%s/%s", llm.Provider, upstreamModelID),
		OpenclawModelName: modelDisplayName(pm),
		OpenclawAPIKey:    llm.openclawAPIKey(),
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

// resolveLLMConfig reads LLM settings from the store (matching old Rust resolve_llm_config).
func resolveLLMConfig(ctx context.Context, s *store.Store) (*llmConfig, error) {
	provider, _ := s.GetSetting(ctx, "llm_provider")
	provider = normalizeLLMProvider(provider)

	baseURL, _ := s.GetSetting(nil, "llm_base_url")
	baseURL = normalizeLLMBaseURL(baseURL)

	model, _ := s.GetSetting(nil, "llm_model")
	model = normalizeLLMModel(model)

	credentialMode, _ := s.GetSetting(nil, "llm_credential_mode")
	credentialMode = normalizeLLMCredentialMode(credentialMode)

	// Read API key (try llm_api_key first, then deepseek_api_key).
	// Matches old CubeAPI resolve_llm_config.
	apiKey, _ := s.GetSetting(ctx, "llm_api_key")
	if apiKey == "" {
		apiKey, _ = s.GetSetting(ctx, "deepseek_api_key")
	}
	apiKey = decryptSetting(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("LLM API key is not configured. Configure it on the AgentHub settings page first")
	}

	return &llmConfig{
		Provider:       provider,
		BaseURL:        baseURL,
		Model:          model,
		APIKey:         apiKey,
		CredentialMode: credentialMode,
	}, nil
}

// openclawApplyMode determines whether to do full init or just merge LLM config.
type openclawApplyMode int

const (
	applyModeFullInit openclawApplyMode = iota
	applyModeMergeLLM
)

type openclawApplyOptions struct {
	mode                 openclawApplyMode
	gatewayToken         string
	preserveGatewayToken bool
	configureWecom       bool
	botID                string
	botSecret            string
}

// openclawApplySpec renders the JSON spec handed to the sandbox apply script.
func openclawApplySpec(plan *llmRuntimePlan, opts *openclawApplyOptions) map[string]interface{} {
	modeStr := "merge_llm"
	if opts.mode == applyModeFullInit {
		modeStr = "full_init"
	}
	gatewaySpec := map[string]interface{}{
		"manage":          opts.mode == applyModeFullInit,
		"preserveExisting": opts.preserveGatewayToken,
	}
	// Only include token in spec if it's non-empty (avoids null values in JSON)
	if opts.gatewayToken != "" {
		gatewaySpec["token"] = opts.gatewayToken
	}
	spec := map[string]interface{}{
		"mode":             modeStr,
		"provider":         plan.UpstreamProvider,
		"baseUrl":          plan.UpstreamBaseURL,
		"apiKey":           plan.OpenclawAPIKey,
		"openclawPrimary":  plan.OpenclawPrimary,
		"upstreamModelId":  plan.UpstreamModelID,
		"modelName":        plan.OpenclawModelName,
		"credentialMode":   plan.CredentialMode,
		"configureWecom":   opts.configureWecom,
		"gateway":          gatewaySpec,
	}
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

// applyOpenclawRuntime writes the OpenClaw runtime config into a sandbox via envd.
// Matches old Rust apply_openclaw_runtime.
func applyOpenclawRuntime(httpClient *http.Client, s *store.Store, sandboxID, domain string, plan *llmRuntimePlan, opts *openclawApplyOptions) (*CommandOutput, error) {
	spec := openclawApplySpec(plan, opts)
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
	if opts.configureWecom {
		if opts.botID != "" {
			envs["OPENCLAW_BOT_ID"] = opts.botID
		}
		if opts.botSecret != "" {
			envs["OPENCLAW_BOT_SECRET"] = opts.botSecret
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

	output, err := runEnvdCommand(httpClient, sandboxID, domain, req)
	if err != nil {
		return nil, fmt.Errorf("envd request failed: %w", err)
	}

	// Retry on config conflict (matching old Rust)
	for i := 0; i < 2; i++ {
		if output.ExitCode == 0 || !isOpenclawConfigConflict(output) {
			break
		}
		output, err = runEnvdCommand(httpClient, sandboxID, domain, req)
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

// llmEgressRule builds the egress rule that injects the LLM API key into
// requests to the LLM provider's base URL. Matches old Rust llm_egress_rule.
func llmEgressRule(llm *llmConfig) (map[string]interface{}, error) {
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
					"header":  "Authorization",
					"secret":  llm.APIKey,
					"format":  format,
				},
			},
		},
	}, nil
}

// agenthubNetworkConfig builds the cube_network_config for sandbox creation.
// In egress credential mode, includes the LLM egress rule with API key injection.
// Matches old Rust agenthub_network_config.
func agenthubNetworkConfig(llm *llmConfig) (map[string]interface{}, error) {
	if !llm.usesEgressCredentials() {
		return nil, nil
	}
	rule, err := llmEgressRule(llm)
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

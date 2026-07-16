// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/redact"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"gorm.io/gorm"
)

const openclawUIPort = 18789

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

// restartOpenclawForInstance restarts the OpenClaw gateway process inside the
// sandbox for the given agent instance. Returns the command output and error.
// Matches old Rust restart_openclaw_for_record.
func restartOpenclawForInstance(inst *store.AgentInstance) (*CommandOutput, error) {
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
	return runEnvdCommand(envdHTTPClient, inst.SandboxID, inst.Domain, req)
}

// upgradeOpenclawForInstance upgrades and restarts the OpenClaw gateway
// inside the sandbox for the given agent instance.
// Matches old Rust upgrade_agent_openclaw.
func upgradeOpenclawForInstance(inst *store.AgentInstance) (*CommandOutput, error) {
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
	return runEnvdCommand(envdHTTPClient, inst.SandboxID, inst.Domain, req)
}

// AgentHubHandler handles agenthub-related HTTP requests.
type AgentHubHandler struct {
	store *store.Store
	cm    CubeMasterClient
}

// NewAgentHubHandler creates a new agenthub handler.
func NewAgentHubHandler(s *store.Store, cm CubeMasterClient) *AgentHubHandler {
	return &AgentHubHandler{store: s, cm: cm}
}

// Register installs the agenthub routes on the given router group.
func (h *AgentHubHandler) Register(r *gin.RouterGroup) {
	// Instances
	r.GET("/agenthub/instances", h.ListInstances)
	r.POST("/agenthub/instances", h.CreateInstance)
	r.DELETE("/agenthub/instances/:agentID", h.DeleteInstance)
	r.GET("/agenthub/instances/:agentID/operations", h.ListOperations)
	r.GET("/agenthub/instances/:agentID/gateway/health", h.GatewayHealth)
	r.POST("/agenthub/instances/:agentID/restart", h.RestartAgent)
	r.POST("/agenthub/instances/:agentID/pause", h.PauseAgent)
	r.POST("/agenthub/instances/:agentID/resume", h.ResumeAgent)
	r.POST("/agenthub/instances/:agentID/upgrade", h.UpgradeAgent)
	r.PUT("/agenthub/instances/:agentID/model", h.UpdateModel)
	r.GET("/agenthub/instances/:agentID/wecom", h.GetWecomConfig)
	r.PUT("/agenthub/instances/:agentID/wecom", h.UpdateWecomConfig)

	// Snapshots
	r.GET("/agenthub/instances/:agentID/snapshots", h.ListSnapshots)
	r.POST("/agenthub/instances/:agentID/snapshots", h.CreateSnapshot)
	r.DELETE("/agenthub/instances/:agentID/snapshots/:snapshotID", h.DeleteSnapshot)
	r.PATCH("/agenthub/instances/:agentID/snapshots/:snapshotID", h.UpdateSnapshot)
	r.POST("/agenthub/instances/:agentID/rollback", h.RollbackAgent)
	r.POST("/agenthub/instances/:agentID/recover", h.RecoverAgent)
	r.POST("/agenthub/instances/:agentID/clone", h.CloneAgent)
	r.POST("/agenthub/instances/:agentID/publish-template", h.PublishTemplate)

	// Templates
	r.GET("/agenthub/templates", h.ListTemplates)
	r.POST("/agenthub/templates/market", h.RegisterMarketTemplate)
	r.PATCH("/agenthub/templates/:templateID", h.UpdateTemplate)
	r.DELETE("/agenthub/templates/:templateID", h.DeleteTemplate)

	// Settings
	r.GET("/agenthub/settings", h.GetSettings)
	r.PUT("/agenthub/settings", h.UpdateSettings)
}

// ListInstances handles GET /agenthub/instances.
//
// Supports pagination via ?limit= and ?offset= query params. limit is
// capped at store.MaxListLimit (200) to prevent OOM on large tables.
// limit <= 0 or missing falls back to store.DefaultListLimit (50).
func (h *AgentHubHandler) ListInstances(c *gin.Context) {
	limit, offset := parsePagination(c)
	instances, err := h.store.ListInstances(c.Request.Context(), limit, offset)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to list instances: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, instances)
}

// DeleteInstance handles DELETE /agenthub/instances/{agentID}.
func (h *AgentHubHandler) DeleteInstance(c *gin.Context) {
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}
	if _, err := h.cm.DeleteSandbox(c.Request.Context(), map[string]string{
		"sandbox_id":    inst.SandboxID,
		"instance_type": inst.Engine,
	}); err != nil {
		// Log but continue — DB record is the source of truth for the UI
		slog.Warn("DeleteInstance: DeleteSandbox failed, continuing with cleanup",
			"agentID", agentID, "sandboxID", inst.SandboxID, "err", err)
	}

	// Clean up host-side OpenClaw state directory (shared_files persistence mode).
	// Best-effort: log errors but don't fail the delete, since the DB record is
	// the source of truth and sandbox deletion already happened.
	if inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
		if err := os.RemoveAll(*inst.OpenclawStatePath); err != nil {
			slog.Warn("DeleteInstance: failed to clean up OpenClaw state directory",
				"agentID", agentID, "path", *inst.OpenclawStatePath, "err", err)
		} else {
			slog.Info("DeleteInstance: cleaned up OpenClaw state directory",
				"agentID", agentID, "path", *inst.OpenclawStatePath)
		}
	}

	if err := h.store.SoftDeleteInstance(c.Request.Context(), agentID); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to delete instance: "+err.Error())
		return
	}
	httputil.WriteNoContent(c)
}

// ListTemplates handles GET /agenthub/templates.
//
// Supports pagination via ?limit= and ?offset= query params. See
// ListInstances for the cap and default behaviour.
func (h *AgentHubHandler) ListTemplates(c *gin.Context) {
	limit, offset := parsePagination(c)
	templates, err := h.store.ListAgentTemplates(c.Request.Context(), limit, offset)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to list templates: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, templates)
}

// DeleteTemplate handles DELETE /agenthub/templates/{templateID}.
func (h *AgentHubHandler) DeleteTemplate(c *gin.Context) {
	templateID := c.Param("templateID")
	if err := h.store.DeleteAgentTemplate(c.Request.Context(), templateID); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to delete template: "+err.Error())
		return
	}
	httputil.WriteNoContent(c)
}

// RestartAgent handles POST /agenthub/instances/{agentID}/restart.
// Restarts the OpenClaw process inside the sandbox via envd .
// Returns AgentSetupResult { exitCode, stdout, stderr } .
func (h *AgentHubHandler) RestartAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	output, err := restartOpenclawForInstance(inst)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to restart: "+err.Error())
		return
	}

	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"exitCode": output.ExitCode,
		"stdout":   output.Stdout,
		"stderr":   output.Stderr,
	})
}

// ListOperations handles GET /agenthub/instances/{agentID}/operations.
func (h *AgentHubHandler) ListOperations(c *gin.Context) {
	agentID := c.Param("agentID")
	ops, err := h.store.ListAgentOperations(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to list operations: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, ops)
}

// PauseAgent handles POST /agenthub/instances/{agentID}/pause.
func (h *AgentHubHandler) PauseAgent(c *gin.Context) { h.sandboxAction(c, "pause") }

// ResumeAgent handles POST /agenthub/instances/{agentID}/resume.
func (h *AgentHubHandler) ResumeAgent(c *gin.Context) { h.sandboxAction(c, "resume") }

func (h *AgentHubHandler) sandboxAction(c *gin.Context, action string) {
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	// Pause and Resume both use update_sandbox API
	if action == "pause" {
		_, err = h.cm.UpdateSandbox(c.Request.Context(), map[string]interface{}{
			"requestID":     fmt.Sprintf("req-%d", time.Now().UnixNano()),
			"sandbox_id":    inst.SandboxID,
			"instance_type": "cubebox",
			"action":        "pause",
		})
	} else {
		// Resume = update_sandbox with action "resume"
		_, err = h.cm.UpdateSandbox(c.Request.Context(), map[string]interface{}{
			"requestID":     fmt.Sprintf("req-%d", time.Now().UnixNano()),
			"sandbox_id":    inst.SandboxID,
			"instance_type": "cubebox",
			"action":        "resume",
			"timeout":       86400,
		})
	}
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to "+action+" sandbox: "+err.Error())
		return
	}

	// Update status: pause → "stopped", resume → "running"
	newStatus := "running"
	if action == "pause" {
		newStatus = "stopped"
	}
	if err := h.store.UpdateInstanceStatus(c.Request.Context(), agentID, newStatus); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to update status: "+err.Error())
		return
	}

	// Return full updated instance
	updated, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil || updated == nil {
		httputil.WriteJSON(c, http.StatusOK, map[string]string{"status": newStatus})
		return
	}
	httputil.WriteJSON(c, http.StatusOK, updated)
}

// UpgradeAgent handles POST /agenthub/instances/{agentID}/upgrade.
// Upgrades OpenClaw to the latest version (via npm/pnpm/pip) and restarts it.
// Matches old Rust upgrade_agent_openclaw.
func (h *AgentHubHandler) UpgradeAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	output, err := upgradeOpenclawForInstance(inst)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to upgrade: "+err.Error())
		return
	}

	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"exitCode": output.ExitCode,
		"stdout":   output.Stdout,
		"stderr":   output.Stderr,
	})
}

// UpdateModel handles PUT /agenthub/instances/{agentID}/model.
func (h *AgentHubHandler) UpdateModel(c *gin.Context) {
	agentID := c.Param("agentID")
	var body struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.store.UpdateInstanceModel(c.Request.Context(), agentID, body.Model); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to update model: "+err.Error())
		return
	}
	httputil.WriteNoContent(c)
}

// GetWecomConfig handles GET /agenthub/instances/{agentID}/wecom.
func (h *AgentHubHandler) GetWecomConfig(c *gin.Context) {
	agentID := c.Param("agentID")
	botID, botSecret, err := h.store.GetAgentWecomConfig(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get wecom config: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, map[string]string{
		"botId":     botID,
		"botSecret": botSecret,
	})
}

// UpdateWecomConfig handles PUT /agenthub/instances/{agentID}/wecom.
func (h *AgentHubHandler) UpdateWecomConfig(c *gin.Context) {
	agentID := c.Param("agentID")
	var body struct {
		BotID     string `json:"botId"`
		BotSecret string `json:"botSecret"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.store.UpdateAgentWecomConfig(c.Request.Context(), agentID, body.BotID, body.BotSecret); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to update wecom config: "+err.Error())
		return
	}
	httputil.WriteNoContent(c)
}

// GetSettings handles GET /agenthub/settings.
func (h *AgentHubHandler) GetSettings(c *gin.Context) {
	ctx := c.Request.Context()

	provider, _ := h.store.GetSetting(ctx, "llm_provider")
	if provider == "" {
		provider = "deepseek"
	}
	baseURL, _ := h.store.GetSetting(ctx, "llm_base_url")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	model, _ := h.store.GetSetting(ctx, "llm_model")
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}
	credentialMode, _ := h.store.GetSetting(ctx, "llm_credential_mode")
	if credentialMode == "" {
		credentialMode = "egress"
	}

	// Check if LLM API key is configured.
	// Matches old CubeAPI: try llm_api_key first, then deepseek_api_key.
	rawApiKey, _ := h.store.GetSetting(ctx, "llm_api_key")
	if rawApiKey == "" {
		rawApiKey, _ = h.store.GetSetting(ctx, "deepseek_api_key")
	}
	apiKey := decryptSetting(rawApiKey)
	apiKeyConfigured := apiKey != ""
	apiKeySource := "none"
	var apiKeyMasked *string
	if apiKeyConfigured {
		apiKeySource = "database"
		masked := maskSecret(apiKey)
		apiKeyMasked = &masked
	}

	gatewayDomain, _ := h.store.GetSetting(ctx, "gateway_domain")
	var gatewayDomainPtr *string
	if gatewayDomain != "" {
		gatewayDomainPtr = &gatewayDomain
	}

	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"deepseekApiKeyConfigured": apiKeyConfigured,
		"deepseekApiKeyMasked":     apiKeyMasked,
		"source":                   apiKeySource,
		"llmProvider":              provider,
		"llmBaseUrl":               baseURL,
		"llmModel":                 model,
		"llmApiKeyConfigured":      apiKeyConfigured,
		"llmApiKeyMasked":          apiKeyMasked,
		"llmApiKeySource":          apiKeySource,
		"llmCredentialMode":        credentialMode,
		"persistenceEnabled":       true,
		"gatewayDomain":            gatewayDomainPtr,
	})
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// decryptSetting returns the plaintext value. If the stored value has the
// enc:v1: prefix, it decrypts it; otherwise it returns the value as-is
// (for backward compatibility with old CubeAPI plaintext storage).
func decryptSetting(stored string) string {
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

// UpdateSettings handles PUT /agenthub/settings.
func (h *AgentHubHandler) UpdateSettings(c *gin.Context) {
	var body map[string]string
	if err := c.ShouldBindJSON(&body); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	// Map frontend field names to DB setting keys
	keyMap := map[string]string{
		"deepseekApiKey":    "deepseek_api_key",
		"llmProvider":       "llm_provider",
		"llmBaseUrl":        "llm_base_url",
		"llmModel":          "llm_model",
		"llmApiKey":         "llm_api_key",
		"llmCredentialMode": "llm_credential_mode",
		"gatewayDomain":     "gateway_domain",
	}
	for jsonKey, value := range body {
		dbKey := jsonKey
		if mapped, ok := keyMap[jsonKey]; ok {
			dbKey = mapped
		}
		if value == "" {
			continue // skip empty values
		}
		// Encrypt API keys before storing
		if dbKey == "deepseek_api_key" || dbKey == "llm_api_key" {
			enc, err := crypto.EncryptSecret(value)
			if err == nil {
				value = enc
			}
		}
		if err := h.store.SetSetting(c.Request.Context(), dbKey, value); err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to update setting "+jsonKey+": "+err.Error())
			return
		}
	}

	// Return updated settings (same format as GET)
	h.GetSettings(c)
}

// ListSnapshots handles GET /agenthub/instances/{agentID}/snapshots.
func (h *AgentHubHandler) ListSnapshots(c *gin.Context) {
	agentID := c.Param("agentID")
	snapshots, err := h.store.ListAgentSnapshots(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to list snapshots: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snapshots)
}

// DeleteSnapshot handles DELETE /agenthub/instances/{agentID}/snapshots/{snapshotID}.
func (h *AgentHubHandler) DeleteSnapshot(c *gin.Context) {
	agentID := c.Param("agentID")
	snapshotID := c.Param("snapshotID")

	// Look up the snapshot before soft-deleting so we can clean up host-side
	// OpenClaw state files (agenthub_state kind). Best-effort cleanup: log
	// errors but don't fail the delete, matching DeleteInstance semantics.
	snap, _ := h.store.GetAgentSnapshot(c.Request.Context(), agentID, snapshotID)
	if snap != nil && snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
		if err := os.RemoveAll(*snap.OpenclawStateSnapshotPath); err != nil {
			slog.Warn("DeleteSnapshot: failed to clean up OpenClaw snapshot directory",
				"agentID", agentID, "snapshotID", snapshotID,
				"path", *snap.OpenclawStateSnapshotPath, "err", err)
		} else {
			slog.Info("DeleteSnapshot: cleaned up OpenClaw snapshot directory",
				"agentID", agentID, "snapshotID", snapshotID,
				"path", *snap.OpenclawStateSnapshotPath)
		}
	}

	if err := h.store.DeleteAgentSnapshot(c.Request.Context(), agentID, snapshotID); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to delete snapshot: "+err.Error())
		return
	}
	httputil.WriteNoContent(c)
}

// GatewayHealth handles GET /agenthub/instances/{agentID}/gateway/health.
// Probes the OpenClaw gateway via the sandbox proxy
// get_agent_gateway_health). Returns { "ready": bool }.
func (h *AgentHubHandler) GatewayHealth(c *gin.Context) {
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	// Probe the OpenClaw gateway through the sandbox proxy
	proxyURL := os.Getenv("AGENTHUB_SANDBOX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1"
	}
	proxyURL = strings.TrimRight(proxyURL, "/")
	probeURL := fmt.Sprintf("%s/sandbox/%s/%d/", proxyURL, inst.SandboxID, openclawUIPort)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(probeURL)
	ready := false
	if err == nil {
		ready = resp.StatusCode >= 200 && resp.StatusCode < 300
		resp.Body.Close()
	}

	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"ready": ready,
	})
}

// Complex operations requiring sandbox creation / openclaw management — stubbed for now.

func (h *AgentHubHandler) CreateInstance(c *gin.Context) {
	var req struct {
		Name            string  `json:"name"`
		Engine          string  `json:"engine"`
		Model           *string `json:"model"`
		TemplateID      *string `json:"templateId"`
		SnapshotID      *string `json:"snapshotId"`
		PersistenceMode *string `json:"persistenceMode"`
		BotID           *string `json:"botId"`
		BotSecret       *string `json:"botSecret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		httputil.WriteError(c, http.StatusBadRequest, "agent name is required")
		return
	}
	if req.Engine != "openclaw" {
		httputil.WriteError(c, http.StatusBadRequest, "only openclaw engine is currently supported")
		return
	}

	hasBotID := req.BotID != nil && strings.TrimSpace(*req.BotID) != ""
	hasBotSecret := req.BotSecret != nil && strings.TrimSpace(*req.BotSecret) != ""
	if hasBotID != hasBotSecret {
		httputil.WriteError(c, http.StatusBadRequest, "Bot ID and Secret must be provided together")
		return
	}
	shouldBindWecom := hasBotID && hasBotSecret

	// Determine persistence mode
	persistenceMode := "full_snapshot"
	if req.PersistenceMode != nil && *req.PersistenceMode != "" {
		pm := strings.TrimSpace(*req.PersistenceMode)
		if pm == "shared_files" || pm == "full_snapshot" {
			persistenceMode = pm
		}
	}

	// Determine rootfs source
	var snapshotID string
	if req.SnapshotID != nil {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	}
	rootfsSourceType := "template"
	rootfsSourceID := ""
	if snapshotID != "" {
		rootfsSourceType = "snapshot"
		rootfsSourceID = snapshotID
	} else {
		if req.TemplateID != nil && strings.TrimSpace(*req.TemplateID) != "" {
			rootfsSourceID = strings.TrimSpace(*req.TemplateID)
		} else {
			rootfsSourceID = "wecom-ds-openclaw"
		}
	}
	templateID := rootfsSourceID
	var explicitTemplateID string
	if req.TemplateID != nil && strings.TrimSpace(*req.TemplateID) != "" {
		explicitTemplateID = strings.TrimSpace(*req.TemplateID)
	}

	// Resolve LLM config from settings
	llmCfg, err := resolveLLMConfig(c.Request.Context(), h.store)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	llmModel := llmCfg.Model
	if req.Model != nil && strings.TrimSpace(*req.Model) != "" {
		llmModel = strings.TrimSpace(*req.Model)
	}

	// Resolve domain from settings
	domain, _ := h.store.GetSetting(c.Request.Context(), "gateway_domain")
	if domain == "" {
		domain = "cube.app"
	}

	// Build egress network config for LLM API key injection
	networkConfig, err := agenthubNetworkConfig(llmCfg)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to build network config: "+err.Error())
		return
	}

	// --- Shared-files persistence setup (matches old Rust) ---
	// For shared_files mode, create a host directory for OpenClaw state and
	// prepare a host mount so /root/.openclaw inside the sandbox maps to it.
	sharedFiles := persistenceMode == "shared_files"
	var openclawPersistID, openclawStatePath string
	var templateOpenclawStateSource string

	// Check if the template has a bundled OpenClaw state snapshot path.
	var agentTemplate *store.AgentTemplate
	if rootfsSourceType == "template" {
		agentTemplate, _ = h.store.GetAgentTemplate(c.Request.Context(), templateID)
	}
	if agentTemplate != nil && agentTemplate.SourceAgentID != "market" {
		if snap, _ := h.store.GetAgentSnapshot(c.Request.Context(), agentTemplate.SourceAgentID, agentTemplate.SourceSnapshotID); snap != nil {
			// If the snapshot has a rootfs_snapshot_id, switch to snapshot source.
			if snap.RootfsSnapshotID != nil && *snap.RootfsSnapshotID != "" {
				rootfsSourceType = "snapshot"
				rootfsSourceID = *snap.RootfsSnapshotID
				templateID = *snap.RootfsSnapshotID
			}
			if snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
				templateOpenclawStateSource = *snap.OpenclawStateSnapshotPath
			}
		}
	}

	if sharedFiles {
		openclawPersistID = newOpenclawPersistID()
		statePath, err := prepareOpenclawStateDir(openclawPersistID)
		if err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
		openclawStatePath = statePath
		// If template has an OpenClaw state snapshot, copy it to the new state dir.
		if templateOpenclawStateSource != "" {
			if err := copyOpenclawStateDir(templateOpenclawStateSource, statePath); err != nil {
				slog.Warn("failed to copy OpenClaw state from template", "source", templateOpenclawStateSource, "err", err)
			}
		}
	}

	// Build CubeMaster create sandbox request (matching old Rust format)
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	labels := map[string]string{
		"agenthub":                    "true",
		"agenthub.name":               name,
		"agenthub.engine":             "openclaw",
		"agenthub.persistence_mode":   persistenceMode,
		"agenthub.rootfs_source_type": rootfsSourceType,
		"agenthub.rootfs_source_id":   rootfsSourceID,
	}
	// Build annotations — host-mount goes here (NOT labels) because CubeMaster's
	// injectHostDirMounts reads from req.Annotations["host-mount"].
	annotations := map[string]string{
		"cube.master.appsnapshot.template.id":      templateID,
		"cube.master.appsnapshot.template.version": "v2",
	}
	if sharedFiles {
		mountMeta, err := openclawHostMountMetadata(openclawStatePath)
		if err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
		labels["agenthub.openclaw.persist_id"] = openclawPersistID
		annotations[hostdirMountKey] = mountMeta
	}

	cmReq := map[string]interface{}{
		"requestID":     requestID,
		"instance_type": "cubebox",
		"timeout":       86400,
		"containers":    []interface{}{},
		"exposed_ports": []interface{}{},
		"annotations":   annotations,
		"labels":        labels,
		"network_type":  "tap",
		"auto_pause":    false,
		"auto_resume":   false,
	}
	if networkConfig != nil {
		cmReq["cube_network_config"] = networkConfig
	}
	// Distribution scope: shared_files and template sources are node-local
	// (host mount is on a specific node), so restrict scheduling to that node.
	if scope := agenthubDistributionScope(persistenceMode, rootfsSourceType); scope != nil {
		cmReq["distribution_scope"] = scope
	}

	// Debug: log the request. Use redact.JSON so credential-shaped fields
	// (e.g. cube_network_config's egress rule "secret" carrying the LLM
	// API key) are masked before reaching the log aggregation system.
	// We log at Debug, not Info, so the body is off by default in prod.
	if reqJSON, err := redact.JSON(cmReq); err == nil {
		slog.Debug("CreateSandbox request", "body", string(reqJSON))
	}

	sandboxResp, err := h.cm.CreateSandbox(c.Request.Context(), cmReq)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to create sandbox: "+err.Error())
		return
	}

	// Parse response — CubeMaster returns { "sandbox_id": "...", "ret": {...} }
	var sbResult struct {
		SandboxID string `json:"sandbox_id"`
		Ret       struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(sandboxResp, &sbResult); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse sandbox response: "+err.Error())
		return
	}
	if sbResult.SandboxID == "" {
		httputil.WriteError(c, http.StatusBadGateway, "CubeMaster returned empty sandbox_id")
		return
	}

	sandboxID := sbResult.SandboxID
	agentID := fmt.Sprintf("agent-%s", sandboxID)

	// Apply OpenClaw runtime config (LLM provider, model, gateway token, etc.)
	// llmCfg was already resolved above before sandbox creation.
	//
	// Determine apply mode (matching old Rust has_openclaw_state logic):
	// - If the template is a published agent snapshot (not market) and no wecom:
	//   use merge_llm (preserve existing gateway token from template)
	// - Otherwise: use full_init (generate new gateway token)
	plan := resolveRuntimePlan(llmCfg, llmModel)
	var generatedToken string
	var applyOpts *openclawApplyOptions

	useTemplateFastpath := false
	if rootfsSourceType == "template" && !shouldBindWecom {
		if tmpl, _ := h.store.GetAgentTemplate(c.Request.Context(), templateID); tmpl != nil && tmpl.SourceAgentID != "market" {
			useTemplateFastpath = true
		}
	}
	// Published-template sandboxes already carry OpenClaw state, so the fast
	// path only merges LLM settings. Shared-files mounts and app-market
	// templates start empty and still need a full init.
	// Matches old Rust: has_openclaw_state = use_template_fastpath && (!shared_files || template_openclaw_state_source.is_some())
	hasOpenclawState := useTemplateFastpath && (!sharedFiles || templateOpenclawStateSource != "")

	if shouldBindWecom {
		generatedToken = generateGatewayToken()
		applyOpts = &openclawApplyOptions{
			mode:           applyModeFullInit,
			gatewayToken:   generatedToken,
			configureWecom: true,
			botID:          strings.TrimSpace(*req.BotID),
			botSecret:      strings.TrimSpace(*req.BotSecret),
		}
	} else if hasOpenclawState {
		applyOpts = &openclawApplyOptions{
			mode:                 applyModeMergeLLM,
			preserveGatewayToken: true,
		}
	} else {
		generatedToken = generateGatewayToken()
		applyOpts = &openclawApplyOptions{
			mode:         applyModeFullInit,
			gatewayToken: generatedToken,
		}
	}
	applyOutput, err := applyOpenclawRuntime(envdHTTPClient, h.store, sandboxID, domain, plan, applyOpts)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to apply OpenClaw config: "+err.Error())
		return
	}

	// Read the gateway token back after apply.
	// Priority (matching old Rust): host-side file > sandbox-side poll > generated fallback.
	// OpenClaw may rewrite openclaw.json on startup, so we poll until stable.
	gatewayToken := resolveGatewayToken(envdHTTPClient, sandboxID, domain, openclawStatePath, generatedToken)

	// Build response (matching old Rust AgentInstanceResponse)
	bots := []string{}
	if shouldBindWecom {
		bots = []string{"wecom"}
	}
	botsAvailable := []string{}
	for _, b := range []string{"wecom"} {
		found := false
		for _, active := range bots {
			if active == b {
				found = true
				break
			}
		}
		if !found {
			botsAvailable = append(botsAvailable, b)
		}
	}

	finalTemplateID := templateID
	if explicitTemplateID != "" {
		finalTemplateID = explicitTemplateID
	}

	gatewayURL := fmt.Sprintf("https://18789-%s.%s", sandboxID, domain)

	// Determine envPort based on template image.
	// All-in-One images have a web desktop on 8080;
	// lightweight images have a code interpreter on 49999 and no desktop.
	// Query CubeMaster directly (no CubeAPI dependency).
	envPort := 8080
	if raw, err := h.cm.ListTemplates(c.Request.Context(), templateID, false); err == nil {
		// CubeMaster returns flat structure for single-template query:
		// {ret, template_id, image_info, ...}
		var tmpl struct {
			ImageInfo string `json:"image_info"`
		}
		if json.Unmarshal(raw, &tmpl) == nil {
			if strings.Contains(strings.ToLower(tmpl.ImageInfo), "lightweight") {
				envPort = 49999
			}
		}
	}
	envURL := fmt.Sprintf("http://%d-%s.%s", envPort, sandboxID, domain)

	// gatewayToken was read after applyOpenclawRuntime above
	if gatewayToken != "" {
		gatewayURL = gatewayURL + "#token=" + gatewayToken
	}

	inst := &store.AgentInstance{
		ID:               agentID,
		Name:             name,
		Status:           "running",
		Engine:           "openclaw",
		Env:              "linux",
		Model:            llmModel,
		Version:          "2026.4.5-t.27",
		Bots:             bots,
		BotsAvailable:    botsAvailable,
		Avatar:           name,
		AvatarTone:       "sky",
		SandboxID:        sandboxID,
		TemplateID:       finalTemplateID,
		GatewayURL:       gatewayURL,
		GatewayToken:     gatewayToken,
		EnvURL:           envURL,
		PersistenceMode:  &persistenceMode,
		RootfsSourceType: &rootfsSourceType,
		RootfsSourceID:   &rootfsSourceID,
		Domain:           domain,
		Setup: &store.AgentSetupResult{
			ExitCode: applyOutput.ExitCode,
			Stdout:   applyOutput.Stdout,
			Stderr:   applyOutput.Stderr,
		},
	}
	// Shared-files persistence fields
	if sharedFiles && openclawPersistID != "" {
		inst.OpenclawPersistID = &openclawPersistID
		inst.OpenclawStatePath = &openclawStatePath
	}

	// WeCom config
	if shouldBindWecom {
		botID := strings.TrimSpace(*req.BotID)
		botSecret := strings.TrimSpace(*req.BotSecret)
		inst.WecomConfig = &store.AgentWecomConfig{
			BotID:     botID,
			BotSecret: botSecret,
		}
	}

	if err := h.store.UpsertInstance(c.Request.Context(), inst); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to create instance record: "+err.Error())
		return
	}

	httputil.WriteJSON(c, http.StatusCreated, inst)
}

func (h *AgentHubHandler) RegisterMarketTemplate(c *gin.Context) {
	var req struct {
		TemplateID  string  `json:"templateId"`
		Name        *string `json:"name"`
		Model       *string `json:"model"`
		Version     *string `json:"version"`
		Recommended bool    `json:"recommended"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TemplateID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "templateId is required")
		return
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	model := ""
	if req.Model != nil {
		model = *req.Model
	}
	version := ""
	if req.Version != nil {
		version = *req.Version
	}
	err := h.store.DB().WithContext(c.Request.Context()).Exec(
		`INSERT INTO t_agenthub_template (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, 'market', '', '', ?, ?)`,
		req.TemplateID, name, model, version,
	).Error
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to register template: "+err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, map[string]string{"templateId": req.TemplateID})
}

func (h *AgentHubHandler) UpdateTemplate(c *gin.Context) {
	templateID := c.Param("templateID")
	var req struct {
		Name        *string `json:"name"`
		Recommended *bool   `json:"recommended"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		if err := h.store.DB().WithContext(c.Request.Context()).Exec(
			`UPDATE t_agenthub_template SET name = ? WHERE template_id = ? AND deleted_at IS NULL`,
			*req.Name, templateID,
		).Error; err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to update template: "+err.Error())
			return
		}
	}
	if req.Recommended != nil {
		if err := h.store.DB().WithContext(c.Request.Context()).Exec(
			`UPDATE t_agenthub_template SET recommended = ? WHERE template_id = ? AND deleted_at IS NULL`,
			*req.Recommended, templateID,
		).Error; err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to update template: "+err.Error())
			return
		}
	}
	httputil.WriteNoContent(c)
}

func (h *AgentHubHandler) CreateSnapshot(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		Name *string `json:"name"`
	}
	_ = c.ShouldBindJSON(&req)

	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	displayName := ""
	if req.Name != nil {
		displayName = *req.Name
	}

	// Determine persistence mode.
	persistenceMode := ""
	if inst.PersistenceMode != nil {
		persistenceMode = *inst.PersistenceMode
	}
	sharedFiles := persistenceMode == "shared_files"

	// shared_files mode: the sandbox has a host_dir volume mount which
	// CommitSandbox does not support. Instead, copy the OpenClaw host state
	// directory to a snapshot directory and record it as an
	// "agenthub_state" snapshot. Matches old Rust create_agent_snapshot
	// shared_files branch.
	if sharedFiles && inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
		sourceOpenclawPath := *inst.OpenclawStatePath
		snapshotID := fmt.Sprintf("agenthub-%s", uuid.New().String())
		snapPath := openclawHostSnapshotPath(snapshotID)
		if err := copyOpenclawStateDir(sourceOpenclawPath, snapPath); err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to copy OpenClaw state: "+err.Error())
			return
		}

		// Determine rootfs snapshot ID (base snapshot or rootfs source).
		rootfsSnapshotID := inst.BaseSnapshotID
		if rootfsSnapshotID == "" && inst.RootfsSourceID != nil {
			rootfsSnapshotID = *inst.RootfsSourceID
		}
		if rootfsSnapshotID == "" {
			rootfsSnapshotID = inst.TemplateID
		}
		rootfsSourceType := "template"
		if inst.RootfsSourceType != nil && *inst.RootfsSourceType != "" {
			rootfsSourceType = *inst.RootfsSourceType
		}

		err = h.store.DB().WithContext(c.Request.Context()).Exec(
			`INSERT INTO t_agenthub_snapshot (
				snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path, deleted_at
			) VALUES (?, ?, ?, ?, 'ready', 'agenthub_state', ?, ?, ?, ?, ?, NULL)
			ON DUPLICATE KEY UPDATE
				agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				name = VALUES(name), status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				origin_sandbox_id = VALUES(origin_sandbox_id),
				rootfs_source_type = VALUES(rootfs_source_type),
				rootfs_source_id = VALUES(rootfs_source_id),
				rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
				openclaw_state_snapshot_path = VALUES(openclaw_state_snapshot_path), deleted_at = NULL`,
			snapshotID, agentID, inst.SandboxID, displayName, inst.SandboxID,
			rootfsSourceType, rootfsSnapshotID, rootfsSnapshotID, snapPath,
		).Error
		if err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to record snapshot: "+err.Error())
			return
		}

		_ = h.store.RecordOperation(c.Request.Context(), agentID, inst.SandboxID, "snapshot", "succeeded", "")
		httputil.WriteJSON(c, http.StatusCreated, map[string]interface{}{
			"snapshotID": snapshotID,
			"names":      []string{},
			"status":     "ready",
		})
		return
	}

	// full_snapshot mode: create a CubeMaster snapshot of the entire sandbox.
	// Matches old Rust create_agent_snapshot full_snapshot branch.
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	snapResp, err := h.cm.CreateSnapshot(c.Request.Context(), map[string]interface{}{
		"request_id":     requestID,
		"sandbox_id":     inst.SandboxID,
		"display_name":   displayName,
		"create_request": map[string]interface{}{},
	})
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to create snapshot: "+err.Error())
		return
	}

	// Parse response — snapshot_id is nested in snapshot object
	var snapResult struct {
		Snapshot struct {
			SnapshotID string `json:"snapshot_id"`
			Status     string `json:"status"`
		} `json:"snapshot"`
		Ret struct {
			RetCode int    `json:"ret_code"`
			RetMsg  string `json:"ret_msg"`
		} `json:"ret"`
	}
	_ = json.Unmarshal(snapResp, &snapResult)
	if snapResult.Snapshot.SnapshotID == "" {
		httputil.WriteError(c, http.StatusBadGateway, "CubeMaster returned empty snapshot_id: "+snapResult.Ret.RetMsg)
		return
	}

	snapshotID := snapResult.Snapshot.SnapshotID

	// Insert snapshot record (matching old Rust upsert_snapshot_info)
	err = h.store.DB().WithContext(c.Request.Context()).Exec(
		`INSERT INTO t_agenthub_snapshot (
			snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
			rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, deleted_at
		) VALUES (?, ?, ?, ?, 'ready', 'sandbox', ?, 'snapshot', ?, ?, NULL)
		ON DUPLICATE KEY UPDATE
			agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
			status = VALUES(status), deleted_at = NULL`,
		snapshotID, agentID, inst.SandboxID, req.Name,
		inst.SandboxID, snapshotID, snapshotID,
	).Error
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to record snapshot: "+err.Error())
		return
	}

	// Record operation for "最近操作" history (matching old CubeAPI).
	_ = h.store.RecordOperation(c.Request.Context(), agentID, inst.SandboxID, "snapshot", "succeeded", "")

	httputil.WriteJSON(c, http.StatusCreated, map[string]interface{}{
		"snapshotID": snapshotID,
		"names":      []string{},
		"status":     "ready",
	})
}

func (h *AgentHubHandler) UpdateSnapshot(c *gin.Context) {
	agentID := c.Param("agentID")
	snapshotID := c.Param("snapshotID")
	var req struct {
		Name      *string `json:"name"`
		IsHealthy *bool   `json:"isHealthy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		if err := h.store.DB().WithContext(c.Request.Context()).Exec(
			`UPDATE t_agenthub_snapshot SET name = ? WHERE snapshot_id = ? AND agent_id = ? AND deleted_at IS NULL`,
			*req.Name, snapshotID, agentID,
		).Error; err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to update snapshot: "+err.Error())
			return
		}
	}
	if req.IsHealthy != nil {
		if err := h.store.DB().WithContext(c.Request.Context()).Exec(
			`UPDATE t_agenthub_snapshot SET is_healthy = ? WHERE snapshot_id = ? AND agent_id = ? AND deleted_at IS NULL`,
			*req.IsHealthy, snapshotID, agentID,
		).Error; err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to update snapshot: "+err.Error())
			return
		}
	}
	httputil.WriteNoContent(c)
}

func (h *AgentHubHandler) RollbackAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		SnapshotID string `json:"snapshotId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SnapshotID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "snapshotId is required")
		return
	}

	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	// Look up the snapshot to determine its kind.
	// shared_files / agenthub_state snapshots store OpenClaw state on the host
	// filesystem, not in CubeMaster — they must be restored by copying the host
	// state directory and restarting OpenClaw, not via RollbackSandbox.
	snap, _ := h.store.GetAgentSnapshot(c.Request.Context(), agentID, req.SnapshotID)
	if snap != nil && snap.SnapshotKind != nil && *snap.SnapshotKind == "agenthub_state" {
		if snap.OpenclawStateSnapshotPath == nil || *snap.OpenclawStateSnapshotPath == "" {
			httputil.WriteError(c, http.StatusBadRequest, "snapshot has no OpenClaw state path")
			return
		}
		if inst.OpenclawStatePath == nil || *inst.OpenclawStatePath == "" {
			httputil.WriteError(c, http.StatusBadRequest, "instance has no active OpenClaw state path")
			return
		}
		if err := copyOpenclawStateDir(*snap.OpenclawStateSnapshotPath, *inst.OpenclawStatePath); err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, "failed to restore OpenClaw state: "+err.Error())
			return
		}
		// Restart OpenClaw to pick up the restored state.
		output, restartErr := restartOpenclawForInstance(inst)
		if restartErr != nil || output.ExitCode != 0 {
			errMsg := "unknown error"
			if restartErr != nil {
				errMsg = restartErr.Error()
			} else if output != nil {
				errMsg = output.Stderr
			}
			httputil.WriteError(c, http.StatusBadGateway, "OpenClaw restart failed after state restore: "+errMsg)
			return
		}
	} else {
		// full_snapshot / sandbox snapshot: delegate to CubeMaster.
		_, err = h.cm.RollbackSandbox(c.Request.Context(), inst.SandboxID, map[string]string{
			"snapshot_id": req.SnapshotID,
		})
		if err != nil {
			httputil.WriteError(c, http.StatusBadGateway, "failed to rollback: "+err.Error())
			return
		}
	}

	_ = h.store.UpdateInstanceStatus(c.Request.Context(), agentID, "running")
	// Record operation for "最近操作" history (matching old CubeAPI).
	_ = h.store.RecordOperation(c.Request.Context(), agentID, inst.SandboxID, "rollback", "succeeded", "")
	httputil.WriteJSON(c, http.StatusOK, map[string]string{"status": "rolled-back"})
}

func (h *AgentHubHandler) RecoverAgent(c *gin.Context) {
	// Crash auto-recovery: attempt to bring OpenClaw back to a healthy state.
	// Matches old Rust recover_agent_openclaw exactly:
	//
	//  1. Try a plain restart — enough for transient failures.
	//  2. If restart fails, roll back to the latest known-healthy snapshot.
	//  3. Restart OpenClaw again after rollback.
	agentID := c.Param("agentID")
	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	ctx := c.Request.Context()

	// Step 1: a plain restart is enough for transient failures.
	output, err := restartOpenclawForInstance(inst)
	if err == nil && output.ExitCode == 0 {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "running")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
		httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
			"recovered":  true,
			"method":     "restart",
			"snapshotId": nil,
		})
		return
	}

	restartErr := ""
	if err != nil {
		restartErr = err.Error()
	} else if output != nil {
		restartErr = output.Stderr
	}
	slog.Warn("recover: restart failed, trying snapshot rollback",
		"agentID", agentID, "error", restartErr)

	// Step 2: restart failed — roll back to the latest known-healthy snapshot.
	snapshotID, err := h.store.LatestHealthySnapshot(ctx, agentID)
	if err != nil {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "failed to look up healthy snapshot: "+err.Error())
		httputil.WriteError(c, http.StatusInternalServerError, "failed to look up healthy snapshot: "+err.Error())
		return
	}
	if snapshotID == "" {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "no healthy snapshot available")
		httputil.WriteError(c, http.StatusConflict, "OpenClaw is unhealthy and no healthy snapshot is available to recover from")
		return
	}

	// Step 3: rollback to the healthy snapshot.
	// shared_files / agenthub_state snapshots must be restored on the host
	// filesystem (same logic as RollbackAgent). full_snapshot delegates to
	// CubeMaster RollbackSandbox.
	snap, _ := h.store.GetAgentSnapshot(ctx, agentID, snapshotID)
	if snap != nil && snap.SnapshotKind != nil && *snap.SnapshotKind == "agenthub_state" {
		if snap.OpenclawStateSnapshotPath == nil || *snap.OpenclawStateSnapshotPath == "" {
			_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "healthy snapshot has no OpenClaw state path")
			httputil.WriteError(c, http.StatusInternalServerError, "cannot recover: healthy snapshot has no OpenClaw state path")
			return
		}
		if inst.OpenclawStatePath == nil || *inst.OpenclawStatePath == "" {
			_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "instance has no active OpenClaw state path")
			httputil.WriteError(c, http.StatusInternalServerError, "cannot recover: instance has no active OpenClaw state path")
			return
		}
		if err := copyOpenclawStateDir(*snap.OpenclawStateSnapshotPath, *inst.OpenclawStatePath); err != nil {
			_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "state restore failed: "+err.Error())
			httputil.WriteError(c, http.StatusInternalServerError, "failed to restore OpenClaw state: "+err.Error())
			return
		}
	} else {
		_, err = h.cm.RollbackSandbox(ctx, inst.SandboxID, map[string]string{
			"snapshot_id": snapshotID,
		})
		if err != nil {
			_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
			_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "rollback failed: "+err.Error())
			httputil.WriteError(c, http.StatusBadGateway, "failed to rollback sandbox: "+err.Error())
			return
		}
	}

	// Step 4: restart OpenClaw again after rollback.
	output, err = restartOpenclawForInstance(inst)
	if err != nil || output.ExitCode != 0 {
		postErr := restartErr
		if err != nil {
			postErr = err.Error()
		} else if output != nil {
			postErr = output.Stderr
		}
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "post-rollback restart failed: "+postErr)
		httputil.WriteError(c, http.StatusInternalServerError, "OpenClaw restart failed after rollback: "+postErr)
		return
	}

	_ = h.store.SetBaseSnapshotID(ctx, agentID, snapshotID)
	_ = h.store.UpdateInstanceStatus(ctx, agentID, "running")
	_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"recovered":  true,
		"method":     "rollback",
		"snapshotId": snapshotID,
	})
}

func (h *AgentHubHandler) CloneAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	_ = c.ShouldBindJSON(&req)

	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	// Determine snapshot source.
	// For agenthub_state snapshots (shared_files mode), the CubeMaster rootfs
	// source must be the snapshot's rootfs_snapshot_id (template ID), not the
	// agenthub-xxx ID. We also capture the OpenClaw state snapshot path so we
	// can clone the host state directory for shared_files clones.
	snapshotID := ""
	if req.SnapshotID != nil && strings.TrimSpace(*req.SnapshotID) != "" {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	} else if inst.BaseSnapshotID != "" {
		snapshotID = inst.BaseSnapshotID
	} else if inst.RootfsSourceID != nil && *inst.RootfsSourceID != "" {
		snapshotID = *inst.RootfsSourceID
	} else {
		snapshotID = inst.TemplateID
	}

	// Look up the snapshot (if it's an agenthub snapshot) to resolve the real
	// rootfs source and the OpenClaw state path for shared_files cloning.
	rootfsSnapshotID := snapshotID
	var sourceOpenclawStatePath string
	if req.SnapshotID != nil && strings.TrimSpace(*req.SnapshotID) != "" {
		if snap, _ := h.store.GetAgentSnapshot(c.Request.Context(), agentID, snapshotID); snap != nil {
			if snap.RootfsSnapshotID != nil && *snap.RootfsSnapshotID != "" {
				rootfsSnapshotID = *snap.RootfsSnapshotID
			}
			if snap.OpenclawStateSnapshotPath != nil && *snap.OpenclawStateSnapshotPath != "" {
				sourceOpenclawStatePath = *snap.OpenclawStateSnapshotPath
			}
		}
	}
	// Fallback to the source agent's own openclaw_state_path when no snapshot
	// was specified or the snapshot didn't carry one. (Matches old Rust:
	// .or(record.openclaw_state_path.as_deref()).)
	if sourceOpenclawStatePath == "" && inst.OpenclawStatePath != nil {
		sourceOpenclawStatePath = *inst.OpenclawStatePath
	}

	cloneName := inst.Name + " 临时助手"
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		cloneName = strings.TrimSpace(*req.Name)
	}

	// shared_files clone: create a new host state directory and copy OpenClaw
	// state from the source snapshot. The clone gets its own persist ID and
	// host-mount, matching CreateInstance's shared_files handling.
	cloneSharedFiles := inst.PersistenceMode != nil && *inst.PersistenceMode == "shared_files"
	slog.Info("CloneAgent: debug",
		"agentID", agentID, "cloneSharedFiles", cloneSharedFiles,
		"sourceOpenclawStatePath", sourceOpenclawStatePath,
		"rootfsSnapshotID", rootfsSnapshotID, "snapshotID", snapshotID)
	var cloneOpenclawStatePath string
	var cloneOpenclawPersistID string
	if cloneSharedFiles && sourceOpenclawStatePath != "" {
		cloneOpenclawPersistID = newOpenclawPersistID()
		statePath, err := prepareOpenclawStateDir(cloneOpenclawPersistID)
		if err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
		cloneOpenclawStatePath = statePath
		if err := copyOpenclawStateDir(sourceOpenclawStatePath, cloneOpenclawStatePath); err != nil {
			slog.Warn("CloneAgent: failed to copy OpenClaw state for clone",
				"agentID", agentID, "err", err)
		}
	}

	// Create sandbox via CubeMaster (same format as CreateInstance).
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	annotations := map[string]string{
		"cube.master.appsnapshot.template.id":      rootfsSnapshotID,
		"cube.master.appsnapshot.template.version": "v2",
	}
	labels := map[string]string{
		"agenthub":                    "true",
		"agenthub.name":               cloneName,
		"agenthub.engine":             inst.Engine,
		"agenthub.rootfs_source_type": "snapshot",
		"agenthub.rootfs_source_id":   rootfsSnapshotID,
	}
	if cloneSharedFiles && cloneOpenclawStatePath != "" {
		mountMeta, err := openclawHostMountMetadata(cloneOpenclawStatePath)
		if err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
		labels["agenthub.openclaw.persist_id"] = cloneOpenclawPersistID
		annotations[hostdirMountKey] = mountMeta
	}

	cmReq := map[string]interface{}{
		"requestID":     requestID,
		"instance_type": "cubebox",
		"timeout":       86400,
		"containers":    []interface{}{},
		"exposed_ports": []interface{}{},
		"annotations":   annotations,
		"labels":        labels,
		"network_type":  "tap",
		"auto_pause":    false,
		"auto_resume":   false,
	}
	// Distribution scope: shared_files clones are node-local (host mount).
	if cloneSharedFiles {
		if scope := agenthubDistributionScope("shared_files", "snapshot"); scope != nil {
			cmReq["distribution_scope"] = scope
		}
	}
	// Network config: in egress credential mode, the LLM egress rule (with
	// real API key injection) must be installed on the cloned sandbox too,
	// otherwise OpenClaw falls back to the placeholder key and gets HTTP 401
	// from the LLM provider. (Matches CreateInstance behaviour.)
	cloneLLMCfgForNet, _ := resolveLLMConfig(c.Request.Context(), h.store)
	if cloneLLMCfgForNet != nil {
		if networkConfig, err := agenthubNetworkConfig(cloneLLMCfgForNet); err == nil && networkConfig != nil {
			cmReq["cube_network_config"] = networkConfig
		}
	}

	sandboxResp, err := h.cm.CreateSandbox(c.Request.Context(), cmReq)
	if err != nil {
		// Best-effort cleanup of the host state dir we just created.
		if cloneOpenclawStatePath != "" {
			_ = os.RemoveAll(cloneOpenclawStatePath)
		}
		httputil.WriteError(c, http.StatusBadGateway, "failed to create clone sandbox: "+err.Error())
		return
	}

	var sbResult struct {
		SandboxID string `json:"sandbox_id"`
	}
	_ = json.Unmarshal(sandboxResp, &sbResult)
	if sbResult.SandboxID == "" {
		if cloneOpenclawStatePath != "" {
			_ = os.RemoveAll(cloneOpenclawStatePath)
		}
		httputil.WriteError(c, http.StatusBadGateway, "CubeMaster returned empty sandbox_id")
		return
	}

	// Build clone response (matching old Rust: copy fields from original instance)
	botsAvailable := []string{}
	for _, b := range []string{"wecom"} {
		found := false
		for _, active := range inst.Bots {
			if active == b {
				found = true
				break
			}
		}
		if !found {
			botsAvailable = append(botsAvailable, b)
		}
	}

	gatewayURL := fmt.Sprintf("https://18789-%s.%s", sbResult.SandboxID, inst.Domain)
	// Inherit envPort from the source agent's envUrl.
	envPort := 8080
	if inst.EnvURL != "" {
		if u, err := url.Parse(inst.EnvURL); err == nil {
			if match := regexp.MustCompile(`^(\d+)-`).FindStringSubmatch(u.Hostname()); match != nil {
				if p, err := strconv.Atoi(match[1]); err == nil {
					envPort = p
				}
			}
		}
	}
	envURL := fmt.Sprintf("http://%d-%s.%s", envPort, sbResult.SandboxID, inst.Domain)

	// Apply OpenClaw runtime config.
	// A copied/full-snapshot sandbox already carries OpenClaw state, so we
	// only merge LLM settings and keep its gateway token; a fresh shared-files
	// mount needs a full init with a new token. (Matches old Rust logic.)
	cloneLLMCfg, err := resolveLLMConfig(c.Request.Context(), h.store)
	if err != nil {
		slog.Warn("CloneAgent: failed to resolve LLM config", "err", err)
	}
	cloneGatewayToken := ""
	if err == nil {
		clonePlan := resolveRuntimePlan(cloneLLMCfg, inst.Model)

		// Determine has_openclaw_state: true if full_snapshot (rootfs carries
		// state) or shared_files with a copied state dir.
		copiedOpenclawState := cloneSharedFiles && cloneOpenclawStatePath != "" && sourceOpenclawStatePath != ""
		hasOpenclawState := copiedOpenclawState || !cloneSharedFiles

		var cloneOpts *openclawApplyOptions
		if hasOpenclawState {
			// Preserve existing gateway token from the snapshot.
			cloneOpts = &openclawApplyOptions{
				mode:                 applyModeMergeLLM,
				preserveGatewayToken: true,
			}
		} else {
			// Fresh shared-files mount needs a full init with a new token.
			cloneGatewayToken = generateGatewayToken()
			cloneOpts = &openclawApplyOptions{
				mode:         applyModeFullInit,
				gatewayToken: cloneGatewayToken,
			}
		}

		applyOutput, applyErr := applyOpenclawRuntime(envdHTTPClient, h.store, sbResult.SandboxID, inst.Domain, clonePlan, cloneOpts)
		if applyErr != nil {
			slog.Error("CloneAgent: failed to apply OpenClaw config, killing clone sandbox",
				"agentID", agentID, "sandboxID", sbResult.SandboxID, "err", applyErr)
			// Best-effort kill the clone sandbox since OpenClaw didn't start.
			if _, derr := h.cm.DeleteSandbox(c.Request.Context(), map[string]interface{}{
				"RequestID":     fmt.Sprintf("req-%d", time.Now().UnixNano()),
				"sandbox_id":    sbResult.SandboxID,
				"instance_type": "cubebox",
			}); derr != nil {
				slog.Warn("CloneAgent: best-effort delete failed", "sandboxID", sbResult.SandboxID, "err", derr)
			}
			if cloneOpenclawStatePath != "" {
				_ = os.RemoveAll(cloneOpenclawStatePath)
			}
			httputil.WriteError(c, http.StatusBadGateway, "failed to apply OpenClaw config: "+applyErr.Error())
			return
		}
		_ = applyOutput
	}

	// Wait for OpenClaw to finish booting before reading the gateway token.
	// (Matches old Rust tokio::time::sleep(Duration::from_secs(5)).)
	time.Sleep(5 * time.Second)

	// Read gateway token from the clone sandbox.
	// Priority (matching old Rust): host-side file > sandbox-side poll > fallback.
	// For shared_files clones, read from the clone's own host state path.
	// The fallback is the source agent's token (for merge_llm) or the
	// generated token (for full_init).
	fallbackToken := cloneGatewayToken
	if fallbackToken == "" {
		fallbackToken = inst.GatewayToken
	}
	cloneGatewayToken = resolveGatewayToken(envdHTTPClient, sbResult.SandboxID, inst.Domain, cloneOpenclawStatePath, fallbackToken)
	if cloneGatewayToken != "" {
		gatewayURL = gatewayURL + "#token=" + cloneGatewayToken
	}

	cloneAgentID := fmt.Sprintf("agent-%s", sbResult.SandboxID)
	rootfsSnapshot := "snapshot"
	clone := &store.AgentInstance{
		ID:               cloneAgentID,
		Name:             cloneName,
		Status:           "running",
		Engine:           inst.Engine,
		Env:              inst.Env,
		Model:            inst.Model,
		Version:          inst.Version,
		Bots:             inst.Bots,
		BotsAvailable:    botsAvailable,
		Avatar:           inst.Avatar,
		AvatarTone:       inst.AvatarTone,
		SandboxID:        sbResult.SandboxID,
		TemplateID:       inst.TemplateID,
		GatewayURL:       gatewayURL,
		GatewayToken:     cloneGatewayToken,
		EnvURL:           envURL,
		PersistenceMode:  inst.PersistenceMode,
		RootfsSourceType: &rootfsSnapshot,
		RootfsSourceID:   &rootfsSnapshotID,
		Domain:           inst.Domain,
	}
	if cloneSharedFiles && cloneOpenclawPersistID != "" {
		clone.OpenclawPersistID = &cloneOpenclawPersistID
		clone.OpenclawStatePath = &cloneOpenclawStatePath
	}
	if inst.WecomConfig != nil {
		clone.WecomConfig = &store.AgentWecomConfig{
			BotID:     inst.WecomConfig.BotID,
			BotSecret: inst.WecomConfig.BotSecret,
		}
	}

	if err := h.store.UpsertInstance(c.Request.Context(), clone); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to create clone record: "+err.Error())
		return
	}

	// Record operation for "最近操作" history (matching old CubeAPI).
	_ = h.store.RecordOperation(c.Request.Context(), agentID, inst.SandboxID, "clone", "succeeded", "")

	httputil.WriteJSON(c, http.StatusCreated, clone)
}

func (h *AgentHubHandler) PublishTemplate(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	_ = c.ShouldBindJSON(&req)

	inst, err := h.store.GetInstance(c.Request.Context(), agentID)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		httputil.WriteError(c, http.StatusNotFound, "instance not found")
		return
	}

	ctx := c.Request.Context()
	snapshotID := ""
	if req.SnapshotID != nil {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	}

	// Determine persistence mode for snapshot creation.
	persistenceMode := "full_snapshot"
	if inst.PersistenceMode != nil && *inst.PersistenceMode != "" {
		persistenceMode = *inst.PersistenceMode
	}
	sharedFiles := persistenceMode == "shared_files"

	snapName := ""
	if req.Name != nil {
		snapName = *req.Name
	}

	// If no snapshot_id provided, create one.
	if snapshotID == "" {
		if sharedFiles {
			// Shared-files mode: copy OpenClaw state to a host snapshot directory.
			// Matches old Rust shared_files branch in publish_agent_template.
			sourceOpenclawPath := ""
			if inst.OpenclawStatePath != nil && *inst.OpenclawStatePath != "" {
				sourceOpenclawPath = *inst.OpenclawStatePath
			}
			if sourceOpenclawPath == "" {
				httputil.WriteError(c, http.StatusBadRequest, "current assistant does not have an OpenClaw host state directory")
				return
			}

			snapshotID = fmt.Sprintf("agenthub-%s", uuid.New().String())
			snapPath := openclawHostSnapshotPath(snapshotID)
			if err := copyOpenclawStateDir(sourceOpenclawPath, snapPath); err != nil {
				httputil.WriteError(c, http.StatusInternalServerError, "failed to copy OpenClaw state: "+err.Error())
				return
			}

			// Determine rootfs snapshot ID (base snapshot or rootfs source).
			rootfsSnapshotID := inst.BaseSnapshotID
			if rootfsSnapshotID == "" && inst.RootfsSourceID != nil {
				rootfsSnapshotID = *inst.RootfsSourceID
			}
			if rootfsSnapshotID == "" {
				rootfsSnapshotID = inst.TemplateID
			}
			rootfsSourceType := "template"
			if inst.RootfsSourceType != nil && *inst.RootfsSourceType != "" {
				rootfsSourceType = *inst.RootfsSourceType
			}

			// Record the agenthub_state snapshot in t_agenthub_snapshot.
			// Matches old Rust upsert_agenthub_openclaw_snapshot.
			_ = h.store.DB().WithContext(ctx).Exec(
				`INSERT INTO t_agenthub_snapshot (
				  snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, openclaw_state_snapshot_path, deleted_at
				) VALUES (?, ?, ?, ?, 'ready', 'agenthub_state', ?, ?, ?, ?, ?, NULL)
				ON DUPLICATE KEY UPDATE
				  agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				  name = VALUES(name), status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				  origin_sandbox_id = VALUES(origin_sandbox_id),
				  rootfs_source_type = VALUES(rootfs_source_type),
				  rootfs_source_id = VALUES(rootfs_source_id),
				  rootfs_snapshot_id = VALUES(rootfs_snapshot_id),
				  openclaw_state_snapshot_path = VALUES(openclaw_state_snapshot_path), deleted_at = NULL`,
				snapshotID, agentID, inst.SandboxID, snapName, inst.SandboxID,
				rootfsSourceType, rootfsSnapshotID, rootfsSnapshotID, snapPath,
			).Error
		} else {
			// Full-snapshot mode: create a CubeMaster snapshot of the entire sandbox.
			// Matches old Rust full_snapshot branch → snapshots.create + upsert_snapshot_info.
			snapResp, err := h.cm.CreateSnapshot(ctx, map[string]interface{}{
				"sandbox_id":    inst.SandboxID,
				"instance_type": "cubebox",
			})
			if err != nil {
				httputil.WriteError(c, http.StatusBadGateway, "failed to create snapshot for template: "+err.Error())
				return
			}

			var snapResult struct {
				SnapshotID string `json:"snapshot_id"`
			}
			_ = json.Unmarshal(snapResp, &snapResult)
			snapshotID = snapResult.SnapshotID
			if snapshotID == "" {
				httputil.WriteError(c, http.StatusBadGateway, "CubeMaster returned empty snapshot_id")
				return
			}

			// Record snapshot info in t_agenthub_snapshot.
			_ = h.store.DB().WithContext(ctx).Exec(
				`INSERT INTO t_agenthub_snapshot (
				  snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
				  rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, deleted_at
				) VALUES (?, ?, ?, ?, 'ready', 'sandbox', ?, 'snapshot', ?, ?, NULL)
				ON DUPLICATE KEY UPDATE
				  agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
				  status = VALUES(status), snapshot_kind = VALUES(snapshot_kind),
				  origin_sandbox_id = VALUES(origin_sandbox_id),
				  rootfs_source_type = VALUES(rootfs_source_type),
				  rootfs_source_id = VALUES(rootfs_source_id),
				  rootfs_snapshot_id = VALUES(rootfs_snapshot_id), deleted_at = NULL`,
				snapshotID, agentID, inst.SandboxID, snapName, inst.SandboxID,
				snapshotID, snapshotID,
			).Error
		}
	}

	// Template name: use provided name or fallback to "{agentName} 模板".
	templateName := inst.Name + " 模板"
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		templateName = strings.TrimSpace(*req.Name)
	}
	templateID := fmt.Sprintf("tpl-%s", snapshotID)

	// Determine persistence_mode (nullable).
	var persistenceModePtr *string
	if inst.PersistenceMode != nil && *inst.PersistenceMode != "" {
		persistenceModePtr = inst.PersistenceMode
	}

	// Wrap the template INSERT and the snapshot.published_template_id UPDATE
	// in a single transaction so the two records stay consistent.
	//
	// Review-bot flag (cubesandboxbot): the previous version discarded the
	// UPDATE error with `_ = ... .Error`, leaving t_agenthub_template rows
	// with no back-link to their source snapshot. Either the template and
	// snapshot link are both committed, or neither is.
	txErr := h.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// INSERT into t_agenthub_template with all required fields
		// (matches old Rust publish_template).
		if err := tx.Exec(
			`INSERT INTO t_agenthub_template (
			  template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id,
			  model, version, persistence_mode, recommended, deleted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)
			ON DUPLICATE KEY UPDATE
			  name = VALUES(name), source_agent_id = VALUES(source_agent_id),
			  source_snapshot_id = VALUES(source_snapshot_id), source_sandbox_id = VALUES(source_sandbox_id),
			  model = VALUES(model), version = VALUES(version),
			  persistence_mode = VALUES(persistence_mode), deleted_at = NULL`,
			templateID, templateName, agentID, snapshotID, inst.SandboxID,
			inst.Model, inst.Version, persistenceModePtr,
		).Error; err != nil {
			return fmt.Errorf("insert template: %w", err)
		}

		// Mark snapshot as published (matches old Rust).
		if err := tx.Exec(
			`UPDATE t_agenthub_snapshot SET published_template_id = ? WHERE snapshot_id = ? AND deleted_at IS NULL`,
			templateID, snapshotID,
		).Error; err != nil {
			return fmt.Errorf("update snapshot published_template_id: %w", err)
		}
		return nil
	})
	if txErr != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to publish template: "+txErr.Error())
		return
	}

	// Record operation. Best-effort: a failure here doesn't invalidate the
	// published template, but we log a warning so operators can detect it.
	if opErr := h.store.RecordOperation(ctx, agentID, inst.SandboxID, "publish_template", "succeeded", ""); opErr != nil {
		slog.Warn("PublishTemplate: failed to record operation",
			"agentID", agentID, "templateID", templateID, "err", opErr)
	}

	httputil.WriteJSON(c, http.StatusCreated, map[string]string{
		"templateId": templateID,
		"snapshotId": snapshotID,
	})
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
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

// AgentHubHandler handles agenthub-related HTTP requests.
type AgentHubHandler struct {
	store *store.Store
	cm    *cubemaster.Client
}

// NewAgentHubHandler creates a new agenthub handler.
func NewAgentHubHandler(s *store.Store, cm *cubemaster.Client) *AgentHubHandler {
	return &AgentHubHandler{store: s, cm: cm}
}

// ListInstances handles GET /agenthub/instances.
func (h *AgentHubHandler) ListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.ListInstances(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list instances: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instances)
}

// DeleteInstance handles DELETE /agenthub/instances/{agentID}.
func (h *AgentHubHandler) DeleteInstance(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if _, err := h.cm.DeleteSandbox(r.Context(), map[string]string{
		"sandbox_id":    inst.SandboxID,
		"instance_type": inst.Engine,
	}); err != nil {
		// Log but continue — DB record is the source of truth for the UI
	}

	if err := h.store.SoftDeleteInstance(r.Context(), agentID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete instance: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListTemplates handles GET /agenthub/templates.
func (h *AgentHubHandler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := h.store.ListAgentTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list templates: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, templates)
}

// DeleteTemplate handles DELETE /agenthub/templates/{templateID}.
func (h *AgentHubHandler) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := mux.Vars(r)["templateID"]
	if err := h.store.DeleteAgentTemplate(r.Context(), templateID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete template: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RestartAgent handles POST /agenthub/instances/{agentID}/restart.
// Restarts the OpenClaw process inside the sandbox via envd .
// Returns AgentSetupResult { exitCode, stdout, stderr } .
func (h *AgentHubHandler) RestartAgent(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	output, err := restartOpenclawForInstance(inst)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to restart: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exitCode": output.ExitCode,
		"stdout":   output.Stdout,
		"stderr":   output.Stderr,
	})
}

// ListOperations handles GET /agenthub/instances/{agentID}/operations.
func (h *AgentHubHandler) ListOperations(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	ops, err := h.store.ListAgentOperations(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list operations: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ops)
}

// PauseAgent handles POST /agenthub/instances/{agentID}/pause.
func (h *AgentHubHandler) PauseAgent(w http.ResponseWriter, r *http.Request) {
	h.sandboxAction(w, r, "pause")
}

// ResumeAgent handles POST /agenthub/instances/{agentID}/resume.
func (h *AgentHubHandler) ResumeAgent(w http.ResponseWriter, r *http.Request) {
	h.sandboxAction(w, r, "resume")
}

func (h *AgentHubHandler) sandboxAction(w http.ResponseWriter, r *http.Request, action string) {
	agentID := mux.Vars(r)["agentID"]
	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	// Pause and Resume both use update_sandbox API 
	if action == "pause" {
		_, err = h.cm.UpdateSandbox(r.Context(), map[string]interface{}{
			"request_id":    fmt.Sprintf("req-%d", time.Now().UnixNano()),
			"sandbox_id":    inst.SandboxID,
			"instance_type": "cubebox",
			"action":        "pause",
		})
	} else {
		// Resume = update_sandbox with action "resume" 
		_, err = h.cm.UpdateSandbox(r.Context(), map[string]interface{}{
			"request_id":    fmt.Sprintf("req-%d", time.Now().UnixNano()),
			"sandbox_id":    inst.SandboxID,
			"instance_type": "cubebox",
			"action":        "resume",
			"timeout":       86400,
		})
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to "+action+" sandbox: "+err.Error())
		return
	}

	// Update status: pause → "stopped", resume → "running" 
	newStatus := "running"
	if action == "pause" {
		newStatus = "stopped"
	}
	if err := h.store.UpdateInstanceStatus(r.Context(), agentID, newStatus); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update status: "+err.Error())
		return
	}

	// Return full updated instance 
	updated, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": newStatus})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// UpgradeAgent handles POST /agenthub/instances/{agentID}/upgrade.
func (h *AgentHubHandler) UpgradeAgent(w http.ResponseWriter, r *http.Request) {
	h.RestartAgent(w, r)
}

// UpdateModel handles PUT /agenthub/instances/{agentID}/model.
func (h *AgentHubHandler) UpdateModel(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.store.UpdateInstanceModel(r.Context(), agentID, body.Model); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update model: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetWecomConfig handles GET /agenthub/instances/{agentID}/wecom.
func (h *AgentHubHandler) GetWecomConfig(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	botID, botSecret, err := h.store.GetAgentWecomConfig(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get wecom config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"botId":     botID,
		"botSecret": botSecret,
	})
}

// UpdateWecomConfig handles PUT /agenthub/instances/{agentID}/wecom.
func (h *AgentHubHandler) UpdateWecomConfig(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var body struct {
		BotID     string `json:"botId"`
		BotSecret string `json:"botSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.store.UpdateAgentWecomConfig(r.Context(), agentID, body.BotID, body.BotSecret); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update wecom config: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetSettings handles GET /agenthub/settings.
func (h *AgentHubHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	// Check if LLM API key is configured
	apiKey, _ := h.store.GetSetting(ctx, "llm_api_key")
	if apiKey == "" {
		apiKey, _ = h.store.GetSetting(ctx, "deepseek_api_key")
	}
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
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

// UpdateSettings handles PUT /agenthub/settings.
func (h *AgentHubHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Map frontend field names to DB setting keys
	keyMap := map[string]string{
		"deepseekApiKey":  "deepseek_api_key",
		"llmProvider":     "llm_provider",
		"llmBaseUrl":      "llm_base_url",
		"llmModel":        "llm_model",
		"llmApiKey":       "llm_api_key",
		"llmCredentialMode": "llm_credential_mode",
		"gatewayDomain":   "gateway_domain",
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
		if err := h.store.SetSetting(r.Context(), dbKey, value); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update setting "+jsonKey+": "+err.Error())
			return
		}
	}

	// Return updated settings (same format as GET)
	h.GetSettings(w, r)
}

// ListSnapshots handles GET /agenthub/instances/{agentID}/snapshots.
func (h *AgentHubHandler) ListSnapshots(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	snapshots, err := h.store.ListAgentSnapshots(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list snapshots: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshots)
}

// DeleteSnapshot handles DELETE /agenthub/instances/{agentID}/snapshots/{snapshotID}.
func (h *AgentHubHandler) DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	agentID := vars["agentID"]
	snapshotID := vars["snapshotID"]
	if err := h.store.DeleteAgentSnapshot(r.Context(), agentID, snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete snapshot: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GatewayHealth handles GET /agenthub/instances/{agentID}/gateway/health.
// Probes the OpenClaw gateway via the sandbox proxy 
// get_agent_gateway_health). Returns { "ready": bool }.
func (h *AgentHubHandler) GatewayHealth(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ready": ready,
	})
}

// Complex operations requiring sandbox creation / openclaw management — stubbed for now.

func (h *AgentHubHandler) CreateInstance(w http.ResponseWriter, r *http.Request) {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "agent name is required")
		return
	}
	if req.Engine != "openclaw" {
		writeError(w, http.StatusBadRequest, "only openclaw engine is currently supported")
		return
	}

	hasBotID := req.BotID != nil && strings.TrimSpace(*req.BotID) != ""
	hasBotSecret := req.BotSecret != nil && strings.TrimSpace(*req.BotSecret) != ""
	if hasBotID != hasBotSecret {
		writeError(w, http.StatusBadRequest, "Bot ID and Secret must be provided together")
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
	llmCfg, err := resolveLLMConfig(r.Context(), h.store)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	llmModel := llmCfg.Model
	if req.Model != nil && strings.TrimSpace(*req.Model) != "" {
		llmModel = strings.TrimSpace(*req.Model)
	}

	// Resolve domain from settings
	domain, _ := h.store.GetSetting(r.Context(), "gateway_domain")
	if domain == "" {
		domain = "cube.app"
	}

	// Build egress network config for LLM API key injection
	networkConfig, err := agenthubNetworkConfig(llmCfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build network config: "+err.Error())
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
		agentTemplate, _ = h.store.GetAgentTemplate(r.Context(), templateID)
	}
	if agentTemplate != nil && agentTemplate.SourceAgentID != "market" {
		if snap, _ := h.store.GetAgentSnapshot(r.Context(), agentTemplate.SourceAgentID, agentTemplate.SourceSnapshotID); snap != nil {
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
			writeError(w, http.StatusInternalServerError, err.Error())
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
		"agenthub":                   "true",
		"agenthub.name":              name,
		"agenthub.engine":            "openclaw",
		"agenthub.persistence_mode":  persistenceMode,
		"agenthub.rootfs_source_type": rootfsSourceType,
		"agenthub.rootfs_source_id":  rootfsSourceID,
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
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		labels["agenthub.openclaw.persist_id"] = openclawPersistID
		annotations[hostdirMountKey] = mountMeta
	}

	cmReq := map[string]interface{}{
		"RequestID":      requestID,
		"instance_type":  "cubebox",
		"timeout":        86400,
		"containers":     []interface{}{},
		"exposed_ports":  []interface{}{},
		"annotations":    annotations,
		"labels":             labels,
		"network_type":       "tap",
		"auto_pause":         false,
		"auto_resume":        false,
	}
	if networkConfig != nil {
		cmReq["cube_network_config"] = networkConfig
	}
	// Distribution scope: shared_files and template sources are node-local
	// (host mount is on a specific node), so restrict scheduling to that node.
	if scope := agenthubDistributionScope(persistenceMode, rootfsSourceType); scope != nil {
		cmReq["distribution_scope"] = scope
	}

	// Debug: log the request
	if reqJSON, err := json.Marshal(cmReq); err == nil {
		slog.Info("CreateSandbox request", "body", string(reqJSON))
	}

	sandboxResp, err := h.cm.CreateSandbox(r.Context(), cmReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create sandbox: "+err.Error())
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
		writeError(w, http.StatusInternalServerError, "failed to parse sandbox response: "+err.Error())
		return
	}
	if sbResult.SandboxID == "" {
		writeError(w, http.StatusBadGateway, "CubeMaster returned empty sandbox_id")
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
		if tmpl, _ := h.store.GetAgentTemplate(r.Context(), templateID); tmpl != nil && tmpl.SourceAgentID != "market" {
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
		writeError(w, http.StatusBadGateway, "failed to apply OpenClaw config: "+err.Error())
		return
	}

	// Read the gateway token back from the sandbox after apply.
	// OpenClaw may trigger an in-process reload after the config file changes.
	// Wait 5 seconds for the reload window so the stored URL matches the token
	// the live gateway actually enforces (matching old Rust).
	time.Sleep(5 * time.Second)
	gatewayToken := readOpenclawGatewayToken(envdHTTPClient, sandboxID, domain)
	if gatewayToken == "" {
		gatewayToken = generatedToken // fallback to the token we passed to the script
	}

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
	envPort := 8080
	cubeAPIURL := os.Getenv("CUBE_API_URL")
	if cubeAPIURL == "" {
		cubeAPIURL = "http://127.0.0.1:3000"
	}
	if resp, err := http.Get(fmt.Sprintf("%s/templates/%s", cubeAPIURL, templateID)); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(strings.ToLower(string(body)), "lightweight") {
			envPort = 49999
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

	if err := h.store.UpsertInstance(r.Context(), inst); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create instance record: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, inst)
}

func (h *AgentHubHandler) RegisterMarketTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TemplateID  string  `json:"templateId"`
		Name        *string `json:"name"`
		Model       *string `json:"model"`
		Version     *string `json:"version"`
		Recommended bool    `json:"recommended"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TemplateID == "" {
		writeError(w, http.StatusBadRequest, "templateId is required")
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
	err := h.store.DB().WithContext(r.Context()).Exec(
		`INSERT INTO t_agenthub_template (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, 'market', '', '', ?, ?)`,
		req.TemplateID, name, model, version,
	).Error
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register template: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"templateId": req.TemplateID})
}

func (h *AgentHubHandler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := mux.Vars(r)["templateID"]
	var req struct {
		Name        *string `json:"name"`
		Recommended *bool   `json:"recommended"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		if err := h.store.DB().WithContext(r.Context()).Exec(
			`UPDATE t_agenthub_template SET name = ? WHERE template_id = ? AND deleted_at IS NULL`,
			*req.Name, templateID,
		).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update template: "+err.Error())
			return
		}
	}
	if req.Recommended != nil {
		if err := h.store.DB().WithContext(r.Context()).Exec(
			`UPDATE t_agenthub_template SET recommended = ? WHERE template_id = ? AND deleted_at IS NULL`,
			*req.Recommended, templateID,
		).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update template: "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AgentHubHandler) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var req struct {
		Name *string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	// Create snapshot via CubeMaster (matching old Rust CreateSnapshotRequest format)
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	displayName := ""
	if req.Name != nil {
		displayName = *req.Name
	}
	snapResp, err := h.cm.CreateSnapshot(r.Context(), map[string]interface{}{
		"request_id":    requestID,
		"sandbox_id":    inst.SandboxID,
		"display_name":  displayName,
		"create_request": map[string]interface{}{},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create snapshot: "+err.Error())
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
	json.Unmarshal(snapResp, &snapResult)
	if snapResult.Snapshot.SnapshotID == "" {
		writeError(w, http.StatusBadGateway, "CubeMaster returned empty snapshot_id: "+snapResult.Ret.RetMsg)
		return
	}

	snapshotID := snapResult.Snapshot.SnapshotID

	// Insert snapshot record (matching old Rust upsert_snapshot_info)
	err = h.store.DB().WithContext(r.Context()).Exec(
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
		writeError(w, http.StatusInternalServerError, "failed to record snapshot: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"snapshotID": snapshotID,
		"names":      []string{},
		"status":     "ready",
	})
}

func (h *AgentHubHandler) UpdateSnapshot(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	agentID := vars["agentID"]
	snapshotID := vars["snapshotID"]
	var req struct {
		Name      *string `json:"name"`
		IsHealthy *bool   `json:"isHealthy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name != nil {
		if err := h.store.DB().WithContext(r.Context()).Exec(
			`UPDATE t_agenthub_snapshot SET name = ? WHERE snapshot_id = ? AND agent_id = ? AND deleted_at IS NULL`,
			*req.Name, snapshotID, agentID,
		).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update snapshot: "+err.Error())
			return
		}
	}
	if req.IsHealthy != nil {
		if err := h.store.DB().WithContext(r.Context()).Exec(
			`UPDATE t_agenthub_snapshot SET is_healthy = ? WHERE snapshot_id = ? AND agent_id = ? AND deleted_at IS NULL`,
			*req.IsHealthy, snapshotID, agentID,
		).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update snapshot: "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AgentHubHandler) RollbackAgent(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var req struct {
		SnapshotID string `json:"snapshotId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SnapshotID == "" {
		writeError(w, http.StatusBadRequest, "snapshotId is required")
		return
	}

	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	_, err = h.cm.RollbackSandbox(r.Context(), inst.SandboxID, map[string]string{
		"snapshot_id": req.SnapshotID,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to rollback: "+err.Error())
		return
	}

	_ = h.store.UpdateInstanceStatus(r.Context(), agentID, "running")
	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled-back"})
}

func (h *AgentHubHandler) RecoverAgent(w http.ResponseWriter, r *http.Request) {
	// Crash auto-recovery: attempt to bring OpenClaw back to a healthy state.
	// Matches old Rust recover_agent_openclaw exactly:
	//
	//  1. Try a plain restart — enough for transient failures.
	//  2. If restart fails, roll back to the latest known-healthy snapshot.
	//  3. Restart OpenClaw again after rollback.
	agentID := mux.Vars(r)["agentID"]
	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	ctx := r.Context()

	// Step 1: a plain restart is enough for transient failures.
	output, err := restartOpenclawForInstance(inst)
	if err == nil && output.ExitCode == 0 {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "running")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
		writeJSON(w, http.StatusOK, map[string]interface{}{
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
		writeError(w, http.StatusInternalServerError, "failed to look up healthy snapshot: "+err.Error())
		return
	}
	if snapshotID == "" {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "no healthy snapshot available")
		writeError(w, http.StatusConflict, "OpenClaw is unhealthy and no healthy snapshot is available to recover from")
		return
	}

	// Step 3: rollback to the healthy snapshot.
	_, err = h.cm.RollbackSandbox(ctx, inst.SandboxID, map[string]string{
		"snapshot_id": snapshotID,
	})
	if err != nil {
		_ = h.store.UpdateInstanceStatus(ctx, agentID, "error")
		_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "failed", "rollback failed: "+err.Error())
		writeError(w, http.StatusBadGateway, "failed to rollback sandbox: "+err.Error())
		return
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
		writeError(w, http.StatusInternalServerError, "OpenClaw restart failed after rollback: "+postErr)
		return
	}

	_ = h.store.SetBaseSnapshotID(ctx, agentID, snapshotID)
	_ = h.store.UpdateInstanceStatus(ctx, agentID, "running")
	_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "recover", "succeeded", "")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"recovered":  true,
		"method":     "rollback",
		"snapshotId": snapshotID,
	})
}

func (h *AgentHubHandler) CloneAgent(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	// Determine snapshot source
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

	cloneName := inst.Name + " 临时助手"
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		cloneName = strings.TrimSpace(*req.Name)
	}

	// Create sandbox via CubeMaster (same format as CreateInstance)
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	sandboxResp, err := h.cm.CreateSandbox(r.Context(), map[string]interface{}{
		"RequestID":     requestID,
		"instance_type": "cubebox",
		"timeout":       86400,
		"containers":    []interface{}{},
		"exposed_ports": []interface{}{},
		"annotations": map[string]string{
			"cube.master.appsnapshot.template.id":      snapshotID,
			"cube.master.appsnapshot.template.version": "v2",
		},
		"labels": map[string]string{
			"agenthub":                   "true",
			"agenthub.name":              cloneName,
			"agenthub.engine":            inst.Engine,
			"agenthub.rootfs_source_type": "snapshot",
			"agenthub.rootfs_source_id":  snapshotID,
		},
		"network_type": "tap",
		"auto_pause":   false,
		"auto_resume":  false,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create clone sandbox: "+err.Error())
		return
	}

	var sbResult struct {
		SandboxID string `json:"sandbox_id"`
	}
	json.Unmarshal(sandboxResp, &sbResult)
	if sbResult.SandboxID == "" {
		writeError(w, http.StatusBadGateway, "CubeMaster returned empty sandbox_id")
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

	// Apply OpenClaw runtime config (merge_llm mode for clones)
	cloneLLMCfg, err := resolveLLMConfig(r.Context(), h.store)
	if err == nil {
		clonePlan := resolveRuntimePlan(cloneLLMCfg, inst.Model)
		cloneOpts := &openclawApplyOptions{
			mode:                 applyModeMergeLLM,
			preserveGatewayToken: true,
		}
		applyOpenclawRuntime(envdHTTPClient, h.store, sbResult.SandboxID, inst.Domain, clonePlan, cloneOpts)
	}

	// Read gateway token from the clone sandbox
	cloneGatewayToken := readOpenclawGatewayToken(envdHTTPClient, sbResult.SandboxID, inst.Domain)
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
		TemplateID:       snapshotID,
		GatewayURL:       gatewayURL,
		GatewayToken:     cloneGatewayToken,
		EnvURL:           envURL,
		PersistenceMode:  inst.PersistenceMode,
		RootfsSourceType: &rootfsSnapshot,
		RootfsSourceID:   &snapshotID,
		Domain:           inst.Domain,
	}
	if inst.WecomConfig != nil {
		clone.WecomConfig = &store.AgentWecomConfig{
			BotID:     inst.WecomConfig.BotID,
			BotSecret: inst.WecomConfig.BotSecret,
		}
	}

	if err := h.store.UpsertInstance(r.Context(), clone); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create clone record: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, clone)
}

func (h *AgentHubHandler) PublishTemplate(w http.ResponseWriter, r *http.Request) {
	agentID := mux.Vars(r)["agentID"]
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	inst, err := h.store.GetInstance(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get instance: "+err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	ctx := r.Context()
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
				writeError(w, http.StatusBadRequest, "current assistant does not have an OpenClaw host state directory")
				return
			}

			snapshotID = fmt.Sprintf("agenthub-%s", uuid.New().String())
			snapPath := openclawHostSnapshotPath(snapshotID)
			if err := copyOpenclawStateDir(sourceOpenclawPath, snapPath); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to copy OpenClaw state: "+err.Error())
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
				writeError(w, http.StatusBadGateway, "failed to create snapshot for template: "+err.Error())
				return
			}

			var snapResult struct {
				SnapshotID string `json:"snapshot_id"`
			}
			json.Unmarshal(snapResp, &snapResult)
			snapshotID = snapResult.SnapshotID
			if snapshotID == "" {
				writeError(w, http.StatusBadGateway, "CubeMaster returned empty snapshot_id")
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

	// INSERT into t_agenthub_template with all required fields (matches old Rust publish_template).
	err = h.store.DB().WithContext(ctx).Exec(
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
	).Error
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to publish template: "+err.Error())
		return
	}

	// Mark snapshot as published (matches old Rust).
	_ = h.store.DB().WithContext(ctx).Exec(
		`UPDATE t_agenthub_snapshot SET published_template_id = ? WHERE snapshot_id = ? AND deleted_at IS NULL`,
		templateID, snapshotID,
	).Error

	// Record operation.
	_ = h.store.RecordOperation(ctx, agentID, inst.SandboxID, "publish_template", "succeeded", "")

	writeJSON(w, http.StatusCreated, map[string]string{
		"templateId": templateID,
		"snapshotId": snapshotID,
	})
}

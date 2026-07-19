// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// AgentHubHandler handles agenthub-related HTTP requests.
//
// The handler is a thin HTTP adapter: it binds request parameters, delegates
// the business logic (CubeMaster request assembly, OpenClaw runtime apply,
// failure compensation, DB upsert) to the service layer, and writes the
// response. Heavy orchestration lives in service.AgentHubService /
// service.openclaw so it can be unit-tested without spinning up gin.
type AgentHubHandler struct {
	store *store.Store
	cm    CubeMasterClient
	svc   *service.AgentHubService
}

// NewAgentHubHandler creates a new agenthub handler.
func NewAgentHubHandler(s *store.Store, cm CubeMasterClient) *AgentHubHandler {
	return &AgentHubHandler{
		store: s,
		cm:    cm,
		svc:   service.NewAgentHubService(s, cm),
	}
}

// AgentHubService returns the underlying service. Used by server.go to wire
// the SDK handler's reverse-sync (SDK template deletes must clean up AgentHub
// registrations, same as the old Rust reverse_sync_agenthub_template).
func (h *AgentHubHandler) AgentHubService() *service.AgentHubService { return h.svc }

// writeServiceError maps a service.Error to the corresponding HTTP response.
// For 503 errors that carry a "retry-after:N" code, the Retry-After header
// is set before writing the response body.
func writeServiceError(c *gin.Context, err error) {
	var svcErr *service.Error
	if !errors.As(err, &svcErr) {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	status := svcErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	// 503 with an embedded "retry-after:N" code → set Retry-After header.
	if status == http.StatusServiceUnavailable && strings.HasPrefix(svcErr.Code, "retry-after:") {
		if v := strings.TrimPrefix(svcErr.Code, "retry-after:"); v != "" {
			c.Header("Retry-After", v)
		}
	}
	httputil.WriteError(c, status, svcErr.Message)
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
	if err := h.svc.DeleteInstance(c.Request.Context(), agentID); err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

// compensateDeleteSandbox is a best-effort cleanup used when Agent creation
// fails after the sandbox has already been created by CubeMaster. It deletes
// the sandbox so CPU/memory/quota are not leaked. Errors are logged but not
// returned — the caller already has an error to report to the client.
//
// Kept as a handler method (rather than forwarding to
// service.AgentHubService.CompensateDeleteSandbox) so the contract tests in
// clone_compensate_test.go / compensate_requestid_test.go can construct a
// minimal AgentHubHandler with only a fake cm and no store, and verify the
// DeleteSandbox request shape directly.
//
// See R10.
func (h *AgentHubHandler) compensateDeleteSandbox(ctx context.Context, sandboxID, reason string) {
	if _, err := h.cm.DeleteSandbox(ctx, map[string]interface{}{
		"requestID":     fmt.Sprintf("cubeops-compensate-%s-%d", reason, time.Now().UnixNano()),
		"sandbox_id":    sandboxID,
		"instance_type": service.SDKInstanceType,
	}); err != nil {
		slog.Error("compensateDeleteSandbox: failed to delete sandbox after creation failure",
			"sandboxID", sandboxID, "reason", reason, "err", err)
	} else {
		slog.Info("compensateDeleteSandbox: sandbox deleted after creation failure",
			"sandboxID", sandboxID, "reason", reason)
	}
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
//
// After the AgentHub template registration is soft-deleted, best-effort
// reverse-sync any OTHER AgentHub registrations that referenced the same
// infra template/snapshot id. This mirrors the old Rust
// reverse_sync_agenthub_template and prevents dangling references when an
// infra template is deleted via the AgentHub path.
func (h *AgentHubHandler) DeleteTemplate(c *gin.Context) {
	templateID := c.Param("templateID")
	if err := h.store.DeleteAgentTemplate(c.Request.Context(), templateID); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to delete template: "+err.Error())
		return
	}
	// Best-effort: clean up any other AgentHub registrations pointing at the
	// same infra id. Failures are logged inside the service, never propagated.
	h.svc.ReverseSyncAgentHubTemplate(c.Request.Context(), templateID)
	httputil.WriteNoContent(c)
}

// RestartAgent handles POST /agenthub/instances/{agentID}/restart.
// Restarts the OpenClaw process inside the sandbox via envd.
// Returns AgentSetupResult { exitCode, stdout, stderr }.
func (h *AgentHubHandler) RestartAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	res, err := h.svc.RestartAgent(c.Request.Context(), agentID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"exitCode": res.ExitCode,
		"stdout":   res.Stdout,
		"stderr":   res.Stderr,
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
	inst, err := h.svc.SandboxAction(c.Request.Context(), agentID, action)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, inst)
}

// UpgradeAgent handles POST /agenthub/instances/{agentID}/upgrade.
// Upgrades OpenClaw to the latest version (via npm/pnpm/pip) and restarts it.
// Matches old Rust upgrade_agent_openclaw.
func (h *AgentHubHandler) UpgradeAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	res, err := h.svc.UpgradeAgent(c.Request.Context(), agentID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"exitCode": res.ExitCode,
		"stdout":   res.Stdout,
		"stderr":   res.Stderr,
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
	inst, err := h.svc.UpdateModel(c.Request.Context(), agentID, body.Model)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, inst)
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
	inst, err := h.svc.UpdateWecomConfig(c.Request.Context(), agentID, body.BotID, body.BotSecret)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, inst)
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
	if err := h.svc.DeleteSnapshot(c.Request.Context(), agentID, snapshotID); err != nil {
		writeServiceError(c, err)
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
	model := ""
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}
	templateID := ""
	if req.TemplateID != nil {
		templateID = strings.TrimSpace(*req.TemplateID)
	}
	snapshotID := ""
	if req.SnapshotID != nil {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	}
	persistenceMode := ""
	if req.PersistenceMode != nil {
		persistenceMode = strings.TrimSpace(*req.PersistenceMode)
	}
	botID := ""
	if req.BotID != nil {
		botID = strings.TrimSpace(*req.BotID)
	}
	botSecret := ""
	if req.BotSecret != nil {
		botSecret = strings.TrimSpace(*req.BotSecret)
	}
	res, err := h.svc.CreateInstance(c.Request.Context(), service.CreateInstanceRequest{
		Name:            req.Name,
		Engine:          req.Engine,
		Model:           model,
		TemplateID:      templateID,
		SnapshotID:      snapshotID,
		PersistenceMode: persistenceMode,
		BotID:           botID,
		BotSecret:       botSecret,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, res.Instance)
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
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	res, err := h.svc.CreateSnapshot(c.Request.Context(), service.CreateSnapshotRequest{
		AgentID: agentID,
		Name:    name,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, res.Body)
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
	if err := h.svc.RollbackAgent(c.Request.Context(), agentID, req.SnapshotID); err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, map[string]string{"status": "rolled-back"})
}
func (h *AgentHubHandler) RecoverAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	res, err := h.svc.RecoverAgent(c.Request.Context(), agentID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	body := map[string]interface{}{
		"recovered":  res.Recovered,
		"method":     res.Method,
		"snapshotId": nil,
	}
	if res.SnapshotID != "" {
		body["snapshotId"] = res.SnapshotID
	}
	httputil.WriteJSON(c, http.StatusOK, body)
}
func (h *AgentHubHandler) CloneAgent(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	_ = c.ShouldBindJSON(&req)
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	snapshotID := ""
	if req.SnapshotID != nil {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	}
	res, err := h.svc.CloneAgent(c.Request.Context(), service.CloneAgentRequest{
		AgentID:    agentID,
		Name:       name,
		SnapshotID: snapshotID,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, res.Instance)
}
func (h *AgentHubHandler) PublishTemplate(c *gin.Context) {
	agentID := c.Param("agentID")
	var req struct {
		Name       *string `json:"name"`
		SnapshotID *string `json:"snapshotId"`
	}
	_ = c.ShouldBindJSON(&req)
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	snapshotID := ""
	if req.SnapshotID != nil {
		snapshotID = strings.TrimSpace(*req.SnapshotID)
	}
	res, err := h.svc.PublishTemplate(c.Request.Context(), service.PublishTemplateRequest{
		AgentID:    agentID,
		Name:       name,
		SnapshotID: snapshotID,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, map[string]string{
		"templateId": res.TemplateID,
		"snapshotId": res.SnapshotID,
	})
}

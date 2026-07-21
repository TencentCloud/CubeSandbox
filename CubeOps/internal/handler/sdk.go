// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
)

// DTO / transformation aliases re-exported from package translator so the
// existing handler call sites in this file compile unchanged. The actual
// implementations live in internal/translator/translator.go.
type (
	cmEnvelope          = translator.CMEnvelope
	cmRet               = translator.CMRet
	cmSandboxListItem   = translator.CMSandboxListItem
	cmSandboxDetailItem = translator.CMSandboxDetailItem
	cmSandboxContainer  = translator.CMSandboxContainer
)

var (
	camelCaseJSON                  = translator.CamelCaseJSON
	snakeToCamel                   = translator.SnakeToCamel
	sandboxStateFromInt            = translator.SandboxStateFromInt
	sandboxStateFromRaw            = translator.SandboxStateFromRaw
	parseMemoryMB                  = translator.ParseMemoryMB
	nanosToISO                     = translator.NanosToISO
	rawToISO                       = translator.RawToISO
	sandboxDomain                  = translator.SandboxDomain
	transformSandboxList           = translator.TransformSandboxList
	transformSandboxDetail         = translator.TransformSandboxDetail
	transformTemplateDetail        = translator.TransformTemplateDetail
	transformCreateTemplateRequest = translator.TransformCreateTemplateRequest
	getString                      = translator.GetString
	getFloat                       = translator.GetFloat
	getArray                       = translator.GetArray
)

// SDKHandler serves WebUI SDK data operations by calling CubeMaster directly,
// eliminating the previous CubeOps → CubeAPI reverse-proxy hop.
// Responses are unwrapped from CubeMaster's {ret, data} envelope and field
// names converted from snake_case to camelCase to match the frontend's
// expected E2B-compatible format.
type SDKHandler struct {
	cm          CubeMasterClient
	agenthubSvc *service.AgentHubService // optional; when set, DeleteTemplate triggers AgentHub reverse-sync
}

// NewSDKHandler creates a new SDK handler backed by the CubeMaster client.
func NewSDKHandler(cm CubeMasterClient) *SDKHandler { return &SDKHandler{cm: cm} }

// WithAgentHubService attaches an AgentHubService so that SDK template/snapshot
// deletions can reverse-sync AgentHub registrations (soft-delete AgentHub
// templates that referenced the just-deleted infra resource). Returns the
// receiver for chaining.
func (h *SDKHandler) WithAgentHubService(svc *service.AgentHubService) *SDKHandler {
	h.agenthubSvc = svc
	return h
}

// Register installs the SDK routes on the given router group. The SDK and
// "v2 SDK" (E2B v2 compatible) prefixes share the same handlers; we register
// them on a sub-group so the caller can mount at both prefixes.
func (h *SDKHandler) Register(r *gin.RouterGroup) {
	// Sandboxes
	r.GET("/sandboxes", h.ListSandboxes)
	r.POST("/sandboxes", h.CreateSandbox)
	r.GET("/sandboxes/:id", h.GetSandbox)
	r.DELETE("/sandboxes/:id", h.DeleteSandbox)
	r.GET("/sandboxes/:id/logs", h.GetSandboxLogs)
	r.POST("/sandboxes/:id/timeout", h.SetSandboxTimeout)
	r.POST("/sandboxes/:id/refreshes", h.RefreshSandbox)
	r.POST("/sandboxes/:id/pause", h.PauseSandbox)
	r.POST("/sandboxes/:id/resume", h.ResumeSandbox)
	r.POST("/sandboxes/:id/connect", h.ConnectSandbox)

	// Snapshots
	r.GET("/snapshots", h.ListSnapshots)
	r.POST("/sandboxes/:id/snapshots", h.CreateSnapshot)
	r.POST("/sandboxes/:id/rollback", h.RollbackSandbox)

	// Templates
	// Literal "compat" path must be registered before the {id} catch-all so
	// "compat" isn't matched as a template id. With gin's tree-based router
	// this is automatic as long as we register in the right order.
	r.GET("/templates", h.ListTemplates)
	r.POST("/templates", h.CreateTemplate)
	r.GET("/templates/compat", h.GetTemplateCompat)
	r.POST("/templates/compat/:id/adopt-baseline", h.AdoptTemplateCompatBaseline)
	r.GET("/templates/:id", h.GetTemplate)
	r.POST("/templates/:id", h.RebuildTemplate)
	r.DELETE("/templates/:id", h.DeleteTemplate)
	r.POST("/templates/:id/builds/:buildID", h.StartTemplateBuild)
	r.GET("/templates/:id/builds/:buildID/status", h.GetTemplateBuildStatus)
	r.GET("/templates/:id/builds/:buildID/logs", h.GetTemplateBuildLogs)
}

const sdkInstanceType = "cubebox"

func sdkRequestID() string {
	return fmt.Sprintf("cubeops-sdk-%d", time.Now().UnixNano())
}

// writeCMError maps a CubeMaster client error to the correct HTTP response.
// If the error is a *cubemaster.CMError, it maps ret_code to 404/409/503
// (with Retry-After for retryable errors). All other errors become 502.
// This fixes R11: previously every business error was collapsed to 502,
// losing 404/409/503 semantics that the WebUI relies on for error handling.
func writeCMError(c *gin.Context, err error) {
	var cmErr *cubemaster.CMError
	if errors.As(err, &cmErr) {
		switch {
		case cmErr.IsNotFound():
			httputil.WriteError(c, http.StatusNotFound, cmErr.RetMsg)
		case cmErr.IsConflict() || cmErr.IsCapacity():
			httputil.WriteError(c, http.StatusConflict, cmErr.RetMsg)
		case cmErr.RetryAfter() > 0:
			c.Header("Retry-After", strconv.Itoa(cmErr.RetryAfter()))
			httputil.WriteError(c, http.StatusServiceUnavailable, cmErr.RetMsg)
		default:
			httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", cmErr.RetCode, cmErr.RetMsg))
		}
		return
	}
	httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
}

// longOpTimeout is the per-request deadline for CubeMaster operations that
// can legitimately take well beyond the default 30 s — currently snapshot
// create, snapshot rollback, and template/snapshot delete. These are
// synchronous Cubelet/LVM operations. See R12.
const longOpTimeout = 240 * time.Second

// longOpCtx returns a context derived from the request context with a
// longOpTimeout deadline. It is used for CubeMaster calls that front
// synchronous, slow operations (snapshot create/rollback, template delete).
// The caller MUST use this context (not c.Request.Context()) when calling
// the CubeMaster client, because the HTTP client no longer has a global
// 30 s timeout (R12 fix in cubemaster.New).
func longOpCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), longOpTimeout)
}

// ── Response transformation ─────────────────────────────────────────────────

// writeSDKResponse unwraps CubeMaster's {ret, data} envelope, checks ret_code,
// extracts the data field, converts all keys from snake_case to camelCase,
// and writes the transformed JSON to the response.
// For responses without a data field (action endpoints like pause/resume),
// it returns the raw response with camelCase key conversion.
func writeSDKResponse(c *gin.Context, raw json.RawMessage) {
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not a standard envelope — pass through as-is.
		httputil.WriteRawJSON(c, http.StatusOK, raw)
		return
	}

	// Check ret_code for errors and map to appropriate HTTP status.
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		msg := env.Ret.RetMsg
		switch env.Ret.RetCode {
		case 130404, 404:
			// Not found — matches old CubeAPI map_err → AppError::NotFound
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			// Conflict — matches old CubeAPI map_err → AppError::Conflict
			httputil.WriteError(c, http.StatusConflict, msg)
		case 400:
			httputil.WriteError(c, http.StatusBadRequest, msg)
		case 130490:
			// Sandbox is pausing — retryable. R11 fix: must be 503, not 502.
			c.Header("Retry-After", "2")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// Resume failed — retryable. R11 fix: must be 503, not 502.
			c.Header("Retry-After", "5")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", env.Ret.RetCode, msg))
		}
		return
	}

	// If data field exists, unwrap and transform it.
	if len(env.Data) > 0 && string(env.Data) != "null" {
		transformed := camelCaseJSON(env.Data)
		httputil.WriteRawJSON(c, http.StatusOK, transformed)
		return
	}

	// No data field — transform the entire response (minus ret envelope).
	// This handles action responses like {requestID, ret} → {requestID}.
	transformed := camelCaseJSON(raw)
	// Strip the "ret" field from the output since the frontend doesn't expect it.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(transformed, &m); err == nil {
		delete(m, "ret")
		if len(m) == 0 {
			c.Status(http.StatusOK)
			return
		}
		out, _ := json.Marshal(m)
		httputil.WriteRawJSON(c, http.StatusOK, out)
		return
	}
	httputil.WriteRawJSON(c, http.StatusOK, transformed)
}

// camelCaseJSON is aliased to translator.CamelCaseJSON above.

// writeSDKJobResponse unwraps CubeMaster's {ret, Job} envelope and returns
// the Job field directly as a flat object (matching old CubeAPI to_job()).
// Used by rebuild/create template endpoints where the frontend expects
// a top-level jobID field.
func writeSDKJobResponse(c *gin.Context, raw json.RawMessage) {
	var env struct {
		Ret *cmRet          `json:"ret,omitempty"`
		Job json.RawMessage `json:"Job,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		httputil.WriteRawJSON(c, http.StatusOK, raw)
		return
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		msg := env.Ret.RetMsg
		switch env.Ret.RetCode {
		case 130404, 404:
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			httputil.WriteError(c, http.StatusConflict, msg)
		case 130490:
			// R11 fix: pausing → 503 + Retry-After, not 502.
			c.Header("Retry-After", "2")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// R11 fix: resume failed → 503 + Retry-After, not 502.
			c.Header("Retry-After", "5")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", env.Ret.RetCode, msg))
		}
		return
	}
	if len(env.Job) > 0 && string(env.Job) != "null" {
		transformed := camelCaseJSON(env.Job)
		httputil.WriteRawJSON(c, http.StatusOK, transformed)
		return
	}
	// No Job field — return empty object.
	httputil.WriteRawJSON(c, http.StatusOK, json.RawMessage(`{}`))
}

// ListSandboxes — GET /api/v1/sdk/sandboxes
func (h *SDKHandler) ListSandboxes(c *gin.Context) {
	body := map[string]interface{}{
		"RequestID":     sdkRequestID(),
		"instance_type": sdkInstanceType,
		"start_idx":     0,
		"size":          500,
	}
	// Frontend sends "limit" param; map it to CubeMaster's "size".
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			body["size"] = n
		}
	}
	if v := c.Query("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			body["size"] = n
		}
	}
	if v := c.Query("start_idx"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			body["start_idx"] = n
		}
	}
	raw, err := h.cm.ListSandboxesWithBody(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	transformed := transformSandboxList(raw)
	httputil.WriteJSON(c, http.StatusOK, transformed)
}

// CreateSandbox — POST /api/v1/sdk/sandboxes
func (h *SDKHandler) CreateSandbox(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Transform frontend request to CubeMaster format.
	// Matches old CubeAPI create_sandbox: converts E2B-style request to
	// CubeMaster's CreateCubeSandboxReq with required fields.
	templateID := getString(req, "templateID")
	if templateID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "templateID is required")
		return
	}

	cmReq := map[string]interface{}{
		"requestID":     sdkRequestID(),
		"instance_type": sdkInstanceType,
		"template_id":   templateID,
		"annotations": map[string]string{
			"cube.master.appsnapshot.template.id":      templateID,
			"cube.master.appsnapshot.template.version": "v2",
		},
		"labels":        map[string]string{},
		"volumes":       []interface{}{},
		"containers":    []interface{}{},
		"exposed_ports": []interface{}{},
		"network_type":  "tap",
		"auto_pause":    false,
		"auto_resume":   false,
	}
	// R13 fix: forward autoPause from the WebUI request instead of hardcoding
	// false. The WebUI sends autoPause=true when the user wants idle sandboxes
	// to be paused (not killed) on timeout.
	if v, ok := req["autoPause"].(bool); ok {
		cmReq["auto_pause"] = v
	}
	// R13 fix: forward alias as a label so CubeMaster/Cubelet can display it.
	if alias := getString(req, "alias"); alias != "" {
		labels := cmReq["labels"].(map[string]string)
		labels["alias"] = alias
	}
	// R13 fix: timeout semantics. The old code hardcoded 86400 as a default
	// and only overrode it when timeout > 0. But timeout=0 from the WebUI
	// means "use CubeMaster's default" — we should NOT send 86400 in that
	// case. Instead, only set timeout when the client explicitly provides a
	// positive value; omit the field entirely when 0 or absent so CubeMaster
	// applies its own default.
	if v, ok := getFloat(req, "timeout"); ok && v > 0 {
		cmReq["timeout"] = int(v)
	} else {
		delete(cmReq, "timeout")
	}
	if meta, ok := req["metadata"].(map[string]interface{}); ok && len(meta) > 0 {
		labels := make(map[string]string, len(meta))
		for k, v := range meta {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
		cmReq["labels"] = labels
	}

	raw, err := h.cm.CreateSandbox(c.Request.Context(), cmReq)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// GetSandbox — GET /api/v1/sdk/sandboxes/{id}
func (h *SDKHandler) GetSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	raw, err := h.cm.GetSandbox(c.Request.Context(), sandboxID, sdkInstanceType)
	if err != nil {
		writeCMError(c, err)
		return
	}

	// Map CubeMaster's ret_code to HTTP status. The previous version
	// skipped this and returned 200 + null body whenever the sandbox
	// wasn't found, which violated REST conventions and confused SDK
	// clients (review-bot flag: "Test gap: Confirms existing bug rather
	// than correct behavior"). Matches the ret_code handling used by
	// CreateSandbox and other SDK handlers above.
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		msg := env.Ret.RetMsg
		switch env.Ret.RetCode {
		case 130404, 404:
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			httputil.WriteError(c, http.StatusConflict, msg)
		case 130490:
			// R11 fix: pausing → 503 + Retry-After, not 502.
			c.Header("Retry-After", "2")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// R11 fix: resume failed → 503 + Retry-After, not 502.
			c.Header("Retry-After", "5")
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", env.Ret.RetCode, msg))
		}
		return
	}

	transformed := transformSandboxDetail(raw)
	httputil.WriteJSON(c, http.StatusOK, transformed)
}

// DeleteSandbox — DELETE /api/v1/sdk/sandboxes/{id}
func (h *SDKHandler) DeleteSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	body := map[string]interface{}{
		"requestID":     sdkRequestID(),
		"sandbox_id":    sandboxID,
		"instance_type": sdkInstanceType,
		"sync":          true,
	}
	raw, err := h.cm.DeleteSandbox(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// GetSandboxLogs — GET /api/v1/sdk/sandboxes/{id}/logs
func (h *SDKHandler) GetSandboxLogs(c *gin.Context) {
	sandboxID := c.Param("id")
	body := map[string]interface{}{
		"sandboxID": sandboxID,
		"limit":     100,
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			body["limit"] = n
		}
	}
	if v := c.Query("cursor"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			body["cursor"] = n
		}
	}
	raw, err := h.cm.GetSandboxLogs(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// SetSandboxTimeout — POST /api/v1/sdk/sandboxes/{id}/timeout
func (h *SDKHandler) SetSandboxTimeout(c *gin.Context) {
	sandboxID := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["RequestID"] = sdkRequestID()
	req["sandboxID"] = sandboxID
	req["instanceType"] = sdkInstanceType
	raw, err := h.cm.SetSandboxTimeout(c.Request.Context(), req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// RefreshSandbox — POST /api/v1/sdk/sandboxes/{id}/refreshes
func (h *SDKHandler) RefreshSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["RequestID"] = sdkRequestID()
	req["sandboxID"] = sandboxID
	req["instanceType"] = sdkInstanceType
	raw, err := h.cm.RefreshSandbox(c.Request.Context(), req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// PauseSandbox — POST /api/v1/sdk/sandboxes/{id}/pause
func (h *SDKHandler) PauseSandbox(c *gin.Context) { h.sandboxUpdateAction(c, "pause") }

// ResumeSandbox — POST /api/v1/sdk/sandboxes/{id}/resume
func (h *SDKHandler) ResumeSandbox(c *gin.Context) { h.sandboxUpdateAction(c, "resume") }

func (h *SDKHandler) sandboxUpdateAction(c *gin.Context, action string) {
	sandboxID := c.Param("id")
	body := map[string]interface{}{
		"requestID":     sdkRequestID(),
		"sandbox_id":    sandboxID,
		"instance_type": sdkInstanceType,
		"action":        action,
	}
	raw, err := h.cm.UpdateSandbox(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// ConnectSandbox — POST /api/v1/sdk/sandboxes/{id}/connect
func (h *SDKHandler) ConnectSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	body := map[string]interface{}{
		"requestID":     sdkRequestID(),
		"sandbox_id":    sandboxID,
		"instance_type": sdkInstanceType,
		"timeout":       86400,
	}
	// Allow optional timeout override from request body.
	var req map[string]interface{}
	if c.Request.Body != nil {
		_ = json.NewDecoder(c.Request.Body).Decode(&req)
	}
	if v, ok := req["timeout"]; ok {
		body["timeout"] = v
	}
	raw, err := h.cm.ConnectSandboxWithBody(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// ── Snapshots ──────────────────────────────────────────────────────────────

// ListSnapshots — GET /api/v1/sdk/snapshots
func (h *SDKHandler) ListSnapshots(c *gin.Context) {
	params := map[string]string{
		"request_id":    sdkRequestID(),
		"instance_type": sdkInstanceType,
	}
	for _, k := range []string{"sandbox_id", "name", "status", "limit", "next_token"} {
		if v := c.Query(k); v != "" {
			params[k] = v
		}
	}
	raw, err := h.cm.ListSnapshots(c.Request.Context(), params)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// CreateSnapshot — POST /api/v1/sdk/sandboxes/{id}/snapshots
func (h *SDKHandler) CreateSnapshot(c *gin.Context) {
	sandboxID := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["request_id"] = sdkRequestID()
	req["sandbox_id"] = sandboxID
	ctx, cancel := longOpCtx(c)
	defer cancel()
	raw, err := h.cm.CreateSnapshot(ctx, req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// RollbackSandbox — POST /api/v1/sdk/sandboxes/{id}/rollback
func (h *SDKHandler) RollbackSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["request_id"] = sdkRequestID()
	req["instance_type"] = sdkInstanceType
	ctx, cancel := longOpCtx(c)
	defer cancel()
	raw, err := h.cm.RollbackSandbox(ctx, sandboxID, req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// ── Templates ──────────────────────────────────────────────────────────────

// ListTemplates — GET /api/v1/sdk/templates
func (h *SDKHandler) ListTemplates(c *gin.Context) {
	templateID := c.Query("template_id")
	includeReq := c.Query("include_request") == "true"
	raw, err := h.cm.ListTemplates(c.Request.Context(), templateID, includeReq)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// GetTemplate — GET /api/v1/sdk/templates/{id}
func (h *SDKHandler) GetTemplate(c *gin.Context) {
	templateID := c.Param("id")
	raw, err := h.cm.ListTemplates(c.Request.Context(), templateID, true)
	if err != nil {
		writeCMError(c, err)
		return
	}
	transformed := transformTemplateDetail(raw)
	if transformed == nil {
		httputil.WriteError(c, http.StatusNotFound, "template not found")
		return
	}
	httputil.WriteJSON(c, http.StatusOK, transformed)
}

// transformTemplateDetail converts CubeMaster's template detail response to
// the frontend's expected format. Only top-level keys are converted to
// camelCase; the nested `replicas` array and `createRequest` object keep
// their internal snake_case structure (matching old CubeAPI behavior where
// they were passed through as raw serde_json::Value without conversion).

// CreateTemplate — POST /api/v1/sdk/templates
func (h *SDKHandler) CreateTemplate(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate required field.
	image, _ := req["image"].(string)
	if strings.TrimSpace(image) == "" {
		httputil.WriteError(c, http.StatusBadRequest, "image is required")
		return
	}

	// Transform frontend request to CubeMaster format (matches old CubeAPI logic).
	cmReq := transformCreateTemplateRequest(req)
	cmReq["requestID"] = sdkRequestID()
	if _, ok := cmReq["instance_type"]; !ok {
		cmReq["instance_type"] = sdkInstanceType
	}

	raw, err := h.cm.CreateTemplateFromImage(c.Request.Context(), cmReq)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKJobResponse(c, raw)
}

// RebuildTemplate — POST /api/v1/sdk/templates/{id}
func (h *SDKHandler) RebuildTemplate(c *gin.Context) {
	templateID := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["requestID"] = sdkRequestID()
	req["template_id"] = templateID
	raw, err := h.cm.RedoTemplate(c.Request.Context(), req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKJobResponse(c, raw)
}

// DeleteTemplate — DELETE /api/v1/sdk/templates/{id}
//
// After the infra template is deleted via CubeMaster, best-effort reverse-sync
// AgentHub registrations that pointed at it (or at a snapshot with the same
// id). This migrates the old Rust reverse_sync_agenthub_template into CubeOps
// — without it, the AgentHub registry would keep referencing a deleted infra
// template/snapshot, leaving dangling WebUI entries.
func (h *SDKHandler) DeleteTemplate(c *gin.Context) {
	templateID := c.Param("id")
	body := map[string]interface{}{
		"RequestID":     sdkRequestID(),
		"template_id":   templateID,
		"instance_type": sdkInstanceType,
		"sync":          true,
	}
	ctx, cancel := longOpCtx(c)
	defer cancel()
	raw, err := h.cm.DeleteTemplate(ctx, body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	// Best-effort reverse-sync. Only runs when an AgentHubService is wired
	// (production server wires it via WithAgentHubService; SDK-only tests
	// skip it). Failures are logged inside the service, never propagated.
	if h.agenthubSvc != nil {
		h.agenthubSvc.ReverseSyncAgentHubTemplate(ctx, templateID)
	}
	writeSDKResponse(c, raw)
}

// StartTemplateBuild — POST /api/v1/sdk/templates/{id}/builds/{buildID}
func (h *SDKHandler) StartTemplateBuild(c *gin.Context) {
	buildID := c.Param("buildID")
	var req map[string]interface{}
	if c.Request.Body != nil {
		_ = json.NewDecoder(c.Request.Body).Decode(&req)
	}
	if req == nil {
		req = map[string]interface{}{}
	}
	req["RequestID"] = sdkRequestID()
	raw, err := h.cm.StartTemplateBuild(c.Request.Context(), buildID, req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// GetTemplateBuildStatus — GET /api/v1/sdk/templates/{id}/builds/{buildID}/status
func (h *SDKHandler) GetTemplateBuildStatus(c *gin.Context) {
	buildID := c.Param("buildID")
	raw, err := h.cm.GetTemplateBuildStatus(c.Request.Context(), buildID)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// GetTemplateBuildLogs — GET /api/v1/sdk/templates/{id}/builds/{buildID}/logs
// Matches old CubeAPI behavior: reuses build status endpoint and formats as log lines.
func (h *SDKHandler) GetTemplateBuildLogs(c *gin.Context) {
	buildID := c.Param("buildID")
	raw, err := h.cm.GetTemplateBuildStatus(c.Request.Context(), buildID)
	if err != nil {
		writeCMError(c, err)
		return
	}

	// Parse the CubeMaster build status response.
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse build status")
		return
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", env.Ret.RetCode, env.Ret.RetMsg))
		return
	}

	// Extract status/progress/message from the response (may be top-level or in data).
	var status, message string
	var progress int
	if len(env.Data) > 0 {
		var d map[string]interface{}
		if json.Unmarshal(env.Data, &d) == nil {
			if v, ok := d["status"].(string); ok {
				status = v
			}
			if v, ok := d["message"].(string); ok {
				message = v
			}
			if v, ok := d["progress"].(float64); ok {
				progress = int(v)
			}
		}
	} else {
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) == nil {
			if v, ok := m["status"].(string); ok {
				status = v
			}
			if v, ok := m["message"].(string); ok {
				message = v
			}
			if v, ok := m["progress"].(float64); ok {
				progress = int(v)
			}
		}
	}

	// Build a single log line (same as old CubeAPI build_log_line).
	line := fmt.Sprintf("[%s] progress=%d%%", status, progress)
	if message != "" {
		line = fmt.Sprintf("[%s] %s", status, message)
	}

	httputil.WriteJSON(c, http.StatusOK, map[string]interface{}{
		"buildID":  buildID,
		"status":   status,
		"progress": progress,
		"lines":    []string{line},
	})
}

// GetTemplateCompat — GET /api/v1/sdk/templates/compat
func (h *SDKHandler) GetTemplateCompat(c *gin.Context) {
	raw, err := h.cm.GetTemplateCompat(c.Request.Context())
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// AdoptTemplateCompatBaseline — POST /api/v1/sdk/templates/compat/:id/adopt-baseline
// Adopts UNKNOWN replicas to the current baseline. Matches old CubeAPI
// adopt_template_compat_baseline: sends {action:"adopt_baseline", template_id}
// to CubeMaster and returns {updated: <count>}.
func (h *SDKHandler) AdoptTemplateCompatBaseline(c *gin.Context) {
	templateID := c.Param("id")
	if templateID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "template id is required")
		return
	}
	body := map[string]interface{}{
		"action":      "adopt_baseline",
		"template_id": templateID,
	}
	raw, err := h.cm.AdoptTemplateCompatBaseline(c.Request.Context(), body)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

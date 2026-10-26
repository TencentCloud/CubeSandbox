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
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/translator"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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

// WithAgentHubService attaches an AgentHubService so SDK template/snapshot
// deletions can soft-delete AgentHub templates that reference the removed
// resource. Returns the receiver for chaining.
func (h *SDKHandler) WithAgentHubService(svc *service.AgentHubService) *SDKHandler {
	h.agenthubSvc = svc
	return h
}

// Register installs the SDK and "v2 SDK" (E2B v2 compatible) routes onto a
// shared sub-group so the caller can mount them at both prefixes.
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

// updateTraceAndLogger writes dimensions (InstanceID/InstanceType/RetCode) to
// rt and rebuilds the cached logger so subsequent log lines pick them up.
func updateTraceAndLogger(ctx context.Context, rt *cubelog.RequestTrace, instanceID, instanceType string, retCode int) context.Context {
	if rt == nil {
		return ctx
	}
	if instanceID != "" {
		rt.InstanceID = instanceID
	}
	if instanceType != "" {
		rt.InstanceType = instanceType
	}
	if retCode != 0 {
		rt.RetCode = int64(retCode)
	}
	return logging.WithLogger(ctx, logging.ReNewLogger(ctx))
}

// writeCMError maps a CubeMaster error to an HTTP response: CMError
// ret_code → 404/409/503 (with Retry-After for retryable); other → 502.
func writeCMError(c *gin.Context, err error) {
	ctx := c.Request.Context()
	rt := cubelog.GetTraceInfo(ctx)
	var cmErr *cubemaster.CMError
	if errors.As(err, &cmErr) {
		ctx = updateTraceAndLogger(ctx, rt, "", sdkInstanceType, cmErr.RetCode)
		switch {
		case cmErr.IsNotFound():
			logging.G(ctx).Errorf("sdk: cubemaster not found: %s", cmErr.RetMsg)
			httputil.WriteError(c, http.StatusNotFound, cmErr.RetMsg)
		case cmErr.IsConflict() || cmErr.IsCapacity():
			logging.G(ctx).Errorf("sdk: cubemaster conflict/capacity: %s", cmErr.RetMsg)
			httputil.WriteError(c, http.StatusConflict, cmErr.RetMsg)
		case cmErr.RetryAfter() > 0:
			c.Header("Retry-After", strconv.Itoa(cmErr.RetryAfter()))
			logging.G(ctx).Errorf("sdk: cubemaster retryable error: retryAfter=%d: %s", cmErr.RetryAfter(), cmErr.RetMsg)
			httputil.WriteError(c, http.StatusServiceUnavailable, cmErr.RetMsg)
		default:
			logging.G(ctx).Errorf("sdk: cubemaster error: %s", cmErr.RetMsg)
			httputil.WriteError(c, http.StatusBadGateway, fmt.Sprintf("cubemaster error %d: %s", cmErr.RetCode, cmErr.RetMsg))
		}
		return
	}
	logging.G(ctx).Errorf("sdk: cubemaster transport error: %v", err)
	httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
}

// longOpTimeout is the per-request deadline for CubeMaster operations that
// can legitimately take well beyond the default 30 s — currently snapshot
// create, snapshot rollback, and template/snapshot delete. These are
// synchronous Cubelet/LVM operations.
const longOpTimeout = 240 * time.Second

// longOpCtx returns a request-derived context bounded by longOpTimeout,
// for synchronous slow CubeMaster calls (snapshot/rollback, template delete).
func longOpCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), longOpTimeout)
}

// ── Response transformation ─────────────────────────────────────────────────

// writeSDKResponse unwraps CubeMaster's {ret, data} envelope, checks ret_code,
// and writes the data payload (or the raw body for action endpoints without
// a data field) with snake_case keys converted to camelCase.
func writeSDKResponse(c *gin.Context, raw json.RawMessage) {
	ctx := c.Request.Context()
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not a standard envelope — pass through as-is.
		httputil.WriteRawJSON(c, http.StatusOK, raw)
		return
	}

	// Check ret_code for errors and map to appropriate HTTP status.
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		msg := env.Ret.RetMsg
		ctx = updateTraceAndLogger(ctx, cubelog.GetTraceInfo(ctx), "", sdkInstanceType, env.Ret.RetCode)
		switch env.Ret.RetCode {
		case 130404, 404:
			// Not found — matches old CubeAPI map_err → AppError::NotFound
			logging.G(ctx).Errorf("sdk: cubemaster not found: %s", msg)
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			// Conflict — matches old CubeAPI map_err → AppError::Conflict
			logging.G(ctx).Errorf("sdk: cubemaster conflict: %s", msg)
			httputil.WriteError(c, http.StatusConflict, msg)
		case 400:
			logging.G(ctx).Errorf("sdk: cubemaster bad request: %s", msg)
			httputil.WriteError(c, http.StatusBadRequest, msg)
		case 130490:
			// Sandbox is pausing — retryable. R11 fix: must be 503, not 502.
			c.Header("Retry-After", "2")
			logging.G(ctx).Errorf("sdk: cubemaster sandbox pausing: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// Resume failed — retryable. R11 fix: must be 503, not 502.
			c.Header("Retry-After", "5")
			logging.G(ctx).Errorf("sdk: cubemaster resume failed: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			logging.G(ctx).Errorf("sdk: cubemaster error: %s", msg)
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
	ctx := c.Request.Context()
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
		ctx = updateTraceAndLogger(ctx, cubelog.GetTraceInfo(ctx), "", sdkInstanceType, env.Ret.RetCode)
		switch env.Ret.RetCode {
		case 130404, 404:
			logging.G(ctx).Errorf("sdk: cubemaster not found: %s", msg)
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			logging.G(ctx).Errorf("sdk: cubemaster conflict: %s", msg)
			httputil.WriteError(c, http.StatusConflict, msg)
		case 130490:
			// R11 fix: pausing → 503 + Retry-After, not 502.
			c.Header("Retry-After", "2")
			logging.G(ctx).Errorf("sdk: cubemaster sandbox pausing: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// R11 fix: resume failed → 503 + Retry-After, not 502.
			c.Header("Retry-After", "5")
			logging.G(ctx).Errorf("sdk: cubemaster resume failed: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			logging.G(ctx).Errorf("sdk: cubemaster error: %s", msg)
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
	ctx := c.Request.Context()
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Transform frontend request to CubeMaster format.
	// Matches old CubeAPI create_sandbox: converts E2B-style request to
	// CubeMaster's CreateCubeSandboxReq with required fields.
	templateID := getString(req, "templateID")
	if templateID == "" {
		logging.G(ctx).Errorf("sdk: templateID is required")
		httputil.WriteError(c, http.StatusBadRequest, "templateID is required")
		return
	}

	cmReq := map[string]interface{}{
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
	// forward autoPause from the request so idle sandboxes are paused (not
	// killed) on timeout when the WebUI asks for it.
	if v, ok := req["autoPause"].(bool); ok {
		cmReq["auto_pause"] = v
	}
	//forward alias as a label so CubeMaster/Cubelet can display it.
	if alias := getString(req, "alias"); alias != "" {
		labels := cmReq["labels"].(map[string]string)
		labels["alias"] = alias
	}
	// Only forward timeout when the client sends a positive value; 0 or
	// absent means "use CubeMaster's default", so omit the field instead
	// of sending the previous hardcoded 86400.
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
	ctx := c.Request.Context()
	rt := cubelog.GetTraceInfo(ctx)
	sandboxID := c.Param("id")
	if rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	raw, err := h.cm.GetSandbox(ctx, sandboxID, sdkInstanceType)
	if err != nil {
		writeCMError(c, err)
		return
	}

	// Map ret_code to HTTP status; without it a missing sandbox would
	// return 200 + null, violating REST and confusing SDK clients.
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		msg := env.Ret.RetMsg
		ctx = updateTraceAndLogger(ctx, rt, sandboxID, sdkInstanceType, env.Ret.RetCode)
		switch env.Ret.RetCode {
		case 130404, 404:
			logging.G(ctx).Errorf("sdk: cubemaster not found: %s", msg)
			httputil.WriteError(c, http.StatusNotFound, msg)
		case 130409:
			logging.G(ctx).Errorf("sdk: cubemaster conflict: %s", msg)
			httputil.WriteError(c, http.StatusConflict, msg)
		case 130490:
			// R11 fix: pausing → 503 + Retry-After, not 502.
			c.Header("Retry-After", "2")
			logging.G(ctx).Errorf("sdk: cubemaster sandbox pausing: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		case 130589:
			// R11 fix: resume failed → 503 + Retry-After, not 502.
			c.Header("Retry-After", "5")
			logging.G(ctx).Errorf("sdk: cubemaster resume failed: %s", msg)
			httputil.WriteError(c, http.StatusServiceUnavailable, msg)
		default:
			logging.G(ctx).Errorf("sdk: cubemaster error: %s", msg)
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
	ctx := c.Request.Context()
	sandboxID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["sandboxID"] = sandboxID
	req["instanceType"] = sdkInstanceType
	raw, err := h.cm.SetSandboxTimeout(ctx, req)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKResponse(c, raw)
}

// RefreshSandbox — POST /api/v1/sdk/sandboxes/{id}/refreshes
func (h *SDKHandler) RefreshSandbox(c *gin.Context) {
	ctx := c.Request.Context()
	sandboxID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["sandboxID"] = sandboxID
	req["instanceType"] = sdkInstanceType
	raw, err := h.cm.RefreshSandbox(ctx, req)
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
	ctx := c.Request.Context()
	sandboxID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	body := map[string]interface{}{
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
	raw, err := h.cm.ConnectSandboxWithBody(ctx, body)
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
	ctx := c.Request.Context()
	sandboxID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
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
	ctx := c.Request.Context()
	sandboxID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = sandboxID
		rt.InstanceType = sdkInstanceType
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
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
	ctx := c.Request.Context()
	templateID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = templateID
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	raw, err := h.cm.ListTemplates(ctx, templateID, true)
	if err != nil {
		writeCMError(c, err)
		return
	}
	transformed := transformTemplateDetail(raw)
	if transformed == nil {
		logging.G(ctx).Errorf("sdk: template not found")
		httputil.WriteError(c, http.StatusNotFound, "template not found")
		return
	}
	httputil.WriteJSON(c, http.StatusOK, transformed)
}

// transformTemplateDetail converts CubeMaster's template detail response to
// the frontend's expected format. Only top-level keys become camelCase;
// nested `replicas` and `createRequest` keep their internal snake_case

// CreateTemplate — POST /api/v1/sdk/templates
func (h *SDKHandler) CreateTemplate(c *gin.Context) {
	ctx := c.Request.Context()
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate required field.
	image, _ := req["image"].(string)
	if strings.TrimSpace(image) == "" {
		logging.G(ctx).Errorf("sdk: image is required")
		httputil.WriteError(c, http.StatusBadRequest, "image is required")
		return
	}

	// Transform frontend request to CubeMaster format (matches old CubeAPI logic).
	cmReq := transformCreateTemplateRequest(req)
	if _, ok := cmReq["instance_type"]; !ok {
		cmReq["instance_type"] = sdkInstanceType
	}

	raw, err := h.cm.CreateTemplateFromImage(ctx, cmReq)
	if err != nil {
		writeCMError(c, err)
		return
	}
	writeSDKJobResponse(c, raw)
}

// RebuildTemplate — POST /api/v1/sdk/templates/{id}
func (h *SDKHandler) RebuildTemplate(c *gin.Context) {
	ctx := c.Request.Context()
	templateID := c.Param("id")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = templateID
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(ctx).Errorf("sdk: invalid JSON body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req["template_id"] = templateID
	raw, err := h.cm.RedoTemplate(ctx, req)
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
	ctx := c.Request.Context()
	buildID := c.Param("buildID")
	if rt := cubelog.GetTraceInfo(ctx); rt != nil {
		rt.InstanceID = buildID
		ctx = logging.WithLogger(ctx, logging.ReNewLogger(ctx))
	}
	raw, err := h.cm.GetTemplateBuildStatus(ctx, buildID)
	if err != nil {
		writeCMError(c, err)
		return
	}

	// Parse the CubeMaster build status response.
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		logging.G(ctx).Errorf("sdk: failed to parse build status: %v", err)
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse build status")
		return
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		ctx = updateTraceAndLogger(ctx, cubelog.GetTraceInfo(ctx), buildID, "", env.Ret.RetCode)
		logging.G(ctx).Errorf("sdk: cubemaster error: %s", env.Ret.RetMsg)
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
	ctx := c.Request.Context()
	templateID := c.Param("id")
	if templateID == "" {
		logging.G(ctx).Errorf("sdk: template id is required")
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

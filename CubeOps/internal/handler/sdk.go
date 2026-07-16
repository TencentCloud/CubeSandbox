// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

// SDKHandler serves WebUI SDK data operations by calling CubeMaster directly,
// eliminating the previous CubeOps → CubeAPI reverse-proxy hop.
// Responses are unwrapped from CubeMaster's {ret, data} envelope and field
// names converted from snake_case to camelCase to match the frontend's
// expected E2B-compatible format.
type SDKHandler struct {
	cm CubeMasterClient
}

// NewSDKHandler creates a new SDK handler backed by the CubeMaster client.
func NewSDKHandler(cm CubeMasterClient) *SDKHandler { return &SDKHandler{cm: cm} }

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

// ── Response transformation ─────────────────────────────────────────────────

// cmEnvelope is CubeMaster's standard response wrapper.
type cmEnvelope struct {
	Ret  *cmRet                     `json:"ret,omitempty"`
	Data json.RawMessage            `json:"data,omitempty"`
	Raw  map[string]json.RawMessage `json:"-"`
}

type cmRet struct {
	RetCode int    `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
}

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

// camelCaseJSON recursively converts all object keys in a JSON document
// from snake_case to camelCase. The special suffix "_id" is converted to
// "ID" (not "Id") to match the frontend's TypeScript schema.
func camelCaseJSON(raw json.RawMessage) json.RawMessage {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	transformed := camelCaseValue(v)
	out, err := json.Marshal(transformed)
	if err != nil {
		return raw
	}
	return out
}

func camelCaseValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[snakeToCamel(k)] = camelCaseValue(v)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = camelCaseValue(item)
		}
		return result
	default:
		return v
	}
}

// snakeToCamel converts a snake_case key to camelCase.
// Special case: "id" segment → "ID", "ip" segment → "IP" (not "Id"/"Ip").
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) == 1 {
		return s
	}
	result := parts[0]
	for _, part := range parts[1:] {
		switch part {
		case "id":
			result += "ID"
		case "ip":
			result += "IP"
		default:
			if len(part) > 0 {
				result += strings.ToUpper(part[:1]) + part[1:]
			}
		}
	}
	return result
}

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

// ── Sandboxes ──────────────────────────────────────────────────────────────

// cmSandboxListItem matches CubeMaster's /cube/sandbox/list response items.
type cmSandboxListItem struct {
	SandboxID   string            `json:"sandbox_id"`
	HostID      string            `json:"host_id"`
	Status      json.RawMessage   `json:"status"`     // may be string or int
	StartedAt   json.RawMessage   `json:"started_at"` // may be ISO string or timestamp
	CreateAt    int64             `json:"create_at"`  // Unix nanoseconds, fallback for started_at
	EndAt       json.RawMessage   `json:"end_at"`
	CPUCount    int               `json:"cpu_count"`
	MemoryMB    int               `json:"memory_mb"`
	TemplateID  string            `json:"template_id"`
	Annotations map[string]string `json:"annotations"`
	Labels      map[string]string `json:"labels"`
}

// cmSandboxDetailItem matches CubeMaster's /cube/sandbox/info response items.
type cmSandboxDetailItem struct {
	SandboxID   string               `json:"sandbox_id"`
	Status      int                  `json:"status"`
	HostID      string               `json:"host_id"`
	TemplateID  string               `json:"template_id"`
	Containers  []cmSandboxContainer `json:"containers"`
	Namespace   string               `json:"namespace"`
	EndAt       json.RawMessage      `json:"end_at"`
	Annotations map[string]string    `json:"annotations"`
	Labels      map[string]string    `json:"labels"`
}

type cmSandboxContainer struct {
	ContainerID string `json:"container_id"`
	Status      int    `json:"status"`
	Image       string `json:"image"`
	CreateAt    int64  `json:"create_at"` // nanoseconds
	CPU         string `json:"cpu"`       // e.g. "2000m"
	Mem         string `json:"mem"`       // e.g. "2048Mi"
	Type        string `json:"type"`
	PauseAt     int64  `json:"pause_at"`
}

// sandboxStateFromInt converts CubeMaster integer status to frontend state string.
// CubeMaster: 0=created, 1=running, 2=exited/stopped, 3=unknown, 4=pausing, 5=paused
// Frontend enum: "running" | "paused" | "pausing"
func sandboxStateFromInt(s int) string {
	switch s {
	case 4:
		return "pausing"
	case 5:
		return "paused"
	default:
		return "running"
	}
}

// sandboxStateFromRaw handles status that may be string or int.
func sandboxStateFromRaw(raw json.RawMessage) string {
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(s) {
		case "paused", "pause":
			return "paused"
		case "pausing":
			return "pausing"
		case "1":
			return "running"
		case "2":
			return "running"
		case "4":
			return "pausing"
		case "5":
			return "paused"
		default:
			return "running"
		}
	}
	// Try int.
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return sandboxStateFromInt(n)
	}
	return "running"
}

// parseMemoryMB converts "2048Mi" → 2048, "2048MB" → 2048, "2G" → 2048.
func parseMemoryMB(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "Mi")
	s = strings.TrimSuffix(s, "MI")
	s = strings.TrimSuffix(s, "MB")
	s = strings.TrimSuffix(s, "M")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// nanosToISO converts Unix nanoseconds to RFC 3339 string.
func nanosToISO(nanos int64) string {
	if nanos <= 0 {
		return ""
	}
	seconds := nanos / 1_000_000_000
	t := time.Unix(seconds, 0).UTC()
	return t.Format(time.RFC3339)
}

// rawToISO handles datetime that may be ISO string, milliseconds, or empty.
func rawToISO(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		// Already an ISO string or numeric string.
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			// Numeric — treat as milliseconds.
			if n > 1_000_000_000_000 {
				seconds := n / 1000
				return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
			}
		}
		return s
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		if n > 1_000_000_000_000 {
			// Milliseconds.
			return time.Unix(n/1000, 0).UTC().Format(time.RFC3339)
		}
		if n > 0 {
			// Seconds.
			return time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// sandboxDomain returns the sandbox domain from env (matches old CubeAPI config).
func sandboxDomain() string {
	d := os.Getenv("CUBE_API_SANDBOX_DOMAIN")
	if d == "" {
		d = "cube.app"
	}
	return d
}

// transformSandboxList converts CubeMaster list response to frontend format.
func transformSandboxList(raw json.RawMessage) interface{} {
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return nil
	}

	var items []cmSandboxListItem
	if err := json.Unmarshal(env.Data, &items); err != nil {
		return []interface{}{}
	}

	// Sort by CreateAt descending (newest first).
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreateAt > items[j].CreateAt
	})

	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		// Prefer explicit started_at; fall back to create_at (Unix nanos).
		startedAt := rawToISO(item.StartedAt)
		if startedAt == "" && item.CreateAt > 0 {
			startedAt = nanosToISO(item.CreateAt)
		}
		entry := map[string]interface{}{
			"sandboxID":   item.SandboxID,
			"clientID":    item.HostID,
			"cpuCount":    fmt.Sprintf("%dm", item.CPUCount*1000), // int cores → K8s millicores string
			"memoryMB":    item.MemoryMB,
			"startedAt":   startedAt,
			"endAt":       rawToISO(item.EndAt),
			"state":       sandboxStateFromRaw(item.Status),
			"templateID":  item.TemplateID,
			"envdVersion": item.Annotations["cube.master.components.envd.version"],
			"domain":      sandboxDomain(),
		}
		if item.Labels != nil {
			entry["metadata"] = item.Labels
		}
		result = append(result, entry)
	}
	return result
}

// transformSandboxDetail converts CubeMaster info response to frontend format.
func transformSandboxDetail(raw json.RawMessage) interface{} {
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return nil
	}

	var items []cmSandboxDetailItem
	if err := json.Unmarshal(env.Data, &items); err != nil || len(items) == 0 {
		return nil
	}

	item := items[0]
	// Find primary container (type == "sandbox" or container_id == sandbox_id).
	var primary *cmSandboxContainer
	for i := range item.Containers {
		c := &item.Containers[i]
		if c.Type == "sandbox" || c.ContainerID == item.SandboxID {
			primary = c
			break
		}
	}
	if primary == nil && len(item.Containers) > 0 {
		primary = &item.Containers[0]
	}

	cpuCount := ""
	memoryMB := 0
	startedAt := ""
	if primary != nil {
		cpuCount = primary.CPU // pass through K8s-style millicores string (e.g. "2000m", "128m")
		memoryMB = parseMemoryMB(primary.Mem)
		startedAt = nanosToISO(primary.CreateAt)
	}

	templateID := item.TemplateID
	if templateID == "" {
		templateID = item.Annotations["cube.master.appsnapshot.template.id"]
	}

	result := map[string]interface{}{
		"sandboxID":   item.SandboxID,
		"clientID":    item.HostID,
		"cpuCount":    cpuCount,
		"memoryMB":    memoryMB,
		"startedAt":   startedAt,
		"endAt":       rawToISO(item.EndAt),
		"state":       sandboxStateFromInt(item.Status),
		"templateID":  templateID,
		"envdVersion": item.Annotations["cube.master.components.envd.version"],
		"namespace":   item.Namespace,
		"hostID":      item.HostID,
		"domain":      sandboxDomain(),
	}
	if item.Labels != nil {
		result["metadata"] = item.Labels
	}
	return result
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		"RequestID":     sdkRequestID(),
		"instance_type": sdkInstanceType,
		"template_id":   templateID,
		"timeout":       86400,
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
	if v, ok := getFloat(req, "timeout"); ok && v > 0 {
		cmReq["timeout"] = int(v)
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKResponse(c, raw)
}

// GetSandbox — GET /api/v1/sdk/sandboxes/{id}
func (h *SDKHandler) GetSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	raw, err := h.cm.GetSandbox(c.Request.Context(), sandboxID, sdkInstanceType)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		"RequestID":     sdkRequestID(),
		"sandbox_id":    sandboxID,
		"instance_type": sdkInstanceType,
	}
	raw, err := h.cm.DeleteSandbox(c.Request.Context(), body)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKResponse(c, raw)
}

// ConnectSandbox — POST /api/v1/sdk/sandboxes/{id}/connect
func (h *SDKHandler) ConnectSandbox(c *gin.Context) {
	sandboxID := c.Param("id")
	body := map[string]interface{}{
		"request_id":    sdkRequestID(),
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
	raw, err := h.cm.CreateSnapshot(c.Request.Context(), req)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
	raw, err := h.cm.RollbackSandbox(c.Request.Context(), sandboxID, req)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKResponse(c, raw)
}

// GetTemplate — GET /api/v1/sdk/templates/{id}
func (h *SDKHandler) GetTemplate(c *gin.Context) {
	templateID := c.Param("id")
	raw, err := h.cm.ListTemplates(c.Request.Context(), templateID, true)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
func transformTemplateDetail(raw json.RawMessage) interface{} {
	var env cmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return nil
	}

	// CubeMaster has two response shapes for template queries:
	//  1. Standard envelope: {ret, data: {...}} or {ret, data: [{...}]}
	//  2. Flat response: {ret, template_id, instance_type, ...}
	//     (single-template query returns fields alongside ret, no data wrapper)
	var dataBytes json.RawMessage
	if len(env.Data) > 0 && string(env.Data) != "null" {
		dataBytes = env.Data
		// If data is an array, take the first element.
		var arr []json.RawMessage
		if json.Unmarshal(dataBytes, &arr) == nil {
			if len(arr) == 0 {
				return nil
			}
			dataBytes = arr[0]
		}
	} else {
		// Flat response: strip ret and use the remaining fields.
		var fullMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fullMap); err != nil {
			return nil
		}
		delete(fullMap, "ret")
		dataBytes, _ = json.Marshal(fullMap)
	}

	// Parse as map and convert only top-level keys to camelCase.
	// Values (including replicas[] and createRequest) keep snake_case internals.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(dataBytes, &m); err != nil {
		return nil
	}

	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		camelKey := snakeToCamel(k)
		var val interface{}
		_ = json.Unmarshal(v, &val)
		result[camelKey] = val
	}

	// Promote network fields from createRequest to the top level, matching
	// old CubeAPI behavior where it lifted create_request.network_type to a
	// top-level networkType so the WebUI template detail page can render the
	// "网络类型" column. allowInternetAccess is also lifted from the nested
	// cube_network_config object so the WebUI "公网访问" column reflects the
	// template's egress policy without requiring the frontend to traverse the
	// nested createRequest structure.
	if cr, ok := result["createRequest"].(map[string]interface{}); ok {
		if v, ok := cr["network_type"]; ok && v != nil {
			if _, exists := result["networkType"]; !exists {
				result["networkType"] = v
			}
		}
		if v, ok := cr["cube_network_config"]; ok {
			if cfg, ok := v.(map[string]interface{}); ok {
				if aia, ok := cfg["allowInternetAccess"]; ok && aia != nil {
					if _, exists := result["allowInternetAccess"]; !exists {
						result["allowInternetAccess"] = aia
					}
				}
			}
		}
	}

	return result
}

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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKJobResponse(c, raw)
}

// transformCreateTemplateRequest converts the frontend's create template
// request body to CubeMaster's CreateTemplateFromImageReq format.
// Matches the old CubeAPI create_template service transformation.
func transformCreateTemplateRequest(body map[string]interface{}) map[string]interface{} {
	cmReq := map[string]interface{}{}

	// Direct field mappings (frontend camelCase → CubeMaster snake_case).
	cmReq["source_image_ref"] = strings.TrimSpace(getString(body, "image"))
	cmReq["template_id"] = getString(body, "templateID") // empty = auto-generate
	if v := getString(body, "instanceType"); v != "" {
		cmReq["instance_type"] = v
	}
	if v := getString(body, "writableLayerSize"); v != "" {
		cmReq["writable_layer_size"] = v
	}
	if v := getArray(body, "exposedPorts"); len(v) > 0 {
		cmReq["exposed_ports"] = v
	}
	if v := getString(body, "networkType"); v != "" {
		cmReq["network_type"] = v
	}
	if v := getString(body, "registryUsername"); v != "" {
		cmReq["registry_username"] = v
	}
	if v := getString(body, "registryPassword"); v != "" {
		cmReq["registry_password"] = v
	}
	if v := getArray(body, "nodes"); len(v) > 0 {
		cmReq["distribution_scope"] = v
	}
	if v, ok := body["with_cube_ca"]; ok {
		cmReq["with_cube_ca"] = v
	}

	// Build container_overrides from cpu, memory, command, args, dns, probe, env.
	overrides := map[string]interface{}{}
	hasOverrides := false

	if cmd := getArray(body, "command"); len(cmd) > 0 {
		overrides["command"] = cmd
		hasOverrides = true
	}
	if args := getArray(body, "args"); len(args) > 0 {
		overrides["args"] = args
		hasOverrides = true
	}

	// Resources: cpu (int millicores) → "Nm", memory (int MB) → "NMi"
	resources := map[string]interface{}{}
	if cpu, ok := getFloat(body, "cpu"); ok {
		resources["cpu"] = fmt.Sprintf("%dm", int(cpu))
		hasOverrides = true
	}
	if mem, ok := getFloat(body, "memory"); ok {
		resources["mem"] = fmt.Sprintf("%dMi", int(mem))
		hasOverrides = true
	}
	if len(resources) > 0 {
		overrides["resources"] = resources
	}

	// Probe: probePort (or first exposedPort) + probePath → HTTP GET probe
	// Matches old CubeAPI build_template_probe defaults.
	probePort, hasProbePort := getFloat(body, "probePort")
	if !hasProbePort {
		if ports := getArray(body, "exposedPorts"); len(ports) > 0 {
			if p, ok := ports[0].(float64); ok {
				probePort = p
				hasProbePort = true
			}
		}
	}
	if hasProbePort {
		probePath := getString(body, "probePath")
		if probePath == "" {
			probePath = "/health"
		}
		probe := map[string]interface{}{
			"probe_handler": map[string]interface{}{
				"http_get": map[string]interface{}{
					"path": probePath,
					"port": int(probePort),
				},
			},
			"timeout_ms":        30000,
			"period_ms":         500,
			"success_threshold": 1,
			"failure_threshold": 60,
		}
		overrides["probe"] = probe
		hasOverrides = true
	}

	// DNS: ["8.8.8.8"] → dns_config.servers
	if dns := getArray(body, "dns"); len(dns) > 0 {
		overrides["dns_config"] = map[string]interface{}{
			"servers":  dns,
			"searches": []interface{}{},
		}
		hasOverrides = true
	}

	// Env: ["A=1"] → envs: [{key:"A", value:"1"}]
	if envStrs := getArray(body, "env"); len(envStrs) > 0 {
		envs := make([]map[string]interface{}, 0, len(envStrs))
		for _, e := range envStrs {
			if s, ok := e.(string); ok {
				parts := strings.SplitN(s, "=", 2)
				if len(parts) == 2 {
					envs = append(envs, map[string]interface{}{
						"key":   parts[0],
						"value": parts[1],
					})
				}
			}
		}
		if len(envs) > 0 {
			overrides["envs"] = envs
			hasOverrides = true
		}
	}

	if hasOverrides {
		cmReq["container_overrides"] = overrides
	}

	// Build cube_network_config from allowOut, denyOut, allowInternetAccess.
	netCfg := map[string]interface{}{}
	hasNetCfg := false
	if v, ok := body["allowInternetAccess"]; ok {
		netCfg["allowInternetAccess"] = v
		hasNetCfg = true
	}
	if v := getArray(body, "allowOut"); len(v) > 0 {
		netCfg["allowOut"] = v
		hasNetCfg = true
	}
	if v := getArray(body, "denyOut"); len(v) > 0 {
		netCfg["denyOut"] = v
		hasNetCfg = true
	}
	if hasNetCfg {
		cmReq["cube_network_config"] = netCfg
	}

	return cmReq
}

// --- typed map helpers ---

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) (float64, bool) {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		}
	}
	return 0, false
}

func getArray(m map[string]interface{}, key string) []interface{} {
	if v, ok := m[key]; ok {
		if arr, ok := v.([]interface{}); ok {
			return arr
		}
	}
	return nil
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKJobResponse(c, raw)
}

// DeleteTemplate — DELETE /api/v1/sdk/templates/{id}
func (h *SDKHandler) DeleteTemplate(c *gin.Context) {
	templateID := c.Param("id")
	body := map[string]interface{}{
		"RequestID":     sdkRequestID(),
		"template_id":   templateID,
		"instance_type": sdkInstanceType,
		"sync":          true,
	}
	raw, err := h.cm.DeleteTemplate(c.Request.Context(), body)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKResponse(c, raw)
}

// GetTemplateBuildStatus — GET /api/v1/sdk/templates/{id}/builds/{buildID}/status
func (h *SDKHandler) GetTemplateBuildStatus(c *gin.Context) {
	buildID := c.Param("buildID")
	raw, err := h.cm.GetTemplateBuildStatus(c.Request.Context(), buildID)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
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
		httputil.WriteError(c, http.StatusBadGateway, "cubemaster: "+err.Error())
		return
	}
	writeSDKResponse(c, raw)
}

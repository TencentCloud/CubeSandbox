// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package translator contains the DTO conversion layer that translates
// CubeMaster's snake_case JSON responses into the E2B-compatible
// camelCase format expected by the WebUI / external SDK clients.
//
// These helpers were previously inlined in the handler package
// (internal/handler/sdk.go). They have been extracted here so they can be
// unit-tested independently of gin, and so the handler layer is a thin
// adapter: decode request → call service / CubeMaster → translate → respond.
package translator

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── CubeMaster response envelope ────────────────────────────────────────────

// CMEnvelope is CubeMaster's standard response wrapper.
type CMEnvelope struct {
	Ret  *CMRet                     `json:"ret,omitempty"`
	Data json.RawMessage            `json:"data,omitempty"`
	Raw  map[string]json.RawMessage `json:"-"`
}

// CMRet holds the ret_code / ret_msg pair CubeMaster returns inside Ret.
type CMRet struct {
	RetCode int    `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
}

// ── CubeMaster raw response types (snake_case) ──────────────────────────────

// CMSandboxListItem matches CubeMaster's /cube/sandbox/list response items.
type CMSandboxListItem struct {
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

// CMSandboxDetailItem matches CubeMaster's /cube/sandbox/info response items.
type CMSandboxDetailItem struct {
	SandboxID   string               `json:"sandbox_id"`
	Status      int                  `json:"status"`
	HostID      string               `json:"host_id"`
	TemplateID  string               `json:"template_id"`
	Containers  []CMSandboxContainer `json:"containers"`
	Namespace   string               `json:"namespace"`
	EndAt       json.RawMessage      `json:"end_at"`
	Annotations map[string]string    `json:"annotations"`
	Labels      map[string]string    `json:"labels"`
}

// CMSandboxContainer describes one container inside a CubeMaster sandbox detail.
type CMSandboxContainer struct {
	ContainerID string `json:"container_id"`
	Status      int    `json:"status"`
	Image       string `json:"image"`
	CreateAt    int64  `json:"create_at"` // nanoseconds
	CPU         string `json:"cpu"`       // e.g. "2000m"
	Mem         string `json:"mem"`       // e.g. "2048Mi"
	Type        string `json:"type"`
	PauseAt     int64  `json:"pause_at"`
}

// ── camelCase key conversion ────────────────────────────────────────────────

// CamelCaseJSON recursively converts all object keys in a JSON document
// from snake_case to camelCase. The special suffix "_id" is converted to
// "ID" (not "Id") to match the frontend's TypeScript schema.
func CamelCaseJSON(raw json.RawMessage) json.RawMessage {
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
			result[SnakeToCamel(k)] = camelCaseValue(v)
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

// SnakeToCamel converts a snake_case key to camelCase.
// Special case: "id" segment → "ID", "ip" segment → "IP" (not "Id"/"Ip").
func SnakeToCamel(s string) string {
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

// ── Value / time helpers ────────────────────────────────────────────────────

// SandboxStateFromInt converts CubeMaster integer status to frontend state string.
// CubeMaster: 0=created, 1=running, 2=exited/stopped, 3=unknown, 4=pausing, 5=paused
// Frontend enum: "running" | "paused" | "pausing"
func SandboxStateFromInt(s int) string {
	switch s {
	case 4:
		return "pausing"
	case 5:
		return "paused"
	default:
		return "running"
	}
}

// SandboxStateFromRaw handles status that may be string or int.
func SandboxStateFromRaw(raw json.RawMessage) string {
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
		return SandboxStateFromInt(n)
	}
	return "running"
}

// ParseMemoryMB converts "2048Mi" → 2048, "2048MB" → 2048, "2G" → 2048.
func ParseMemoryMB(s string) int {
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

// NanosToISO converts Unix nanoseconds to RFC 3339 string.
func NanosToISO(nanos int64) string {
	if nanos <= 0 {
		return ""
	}
	seconds := nanos / 1_000_000_000
	t := time.Unix(seconds, 0).UTC()
	return t.Format(time.RFC3339)
}

// RawToISO handles datetime that may be ISO string, milliseconds, or empty.
func RawToISO(raw json.RawMessage) string {
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

// SandboxDomain returns the sandbox domain from env
// (matches old CubeAPI config).
func SandboxDomain() string {
	d := os.Getenv("CUBE_API_SANDBOX_DOMAIN")
	if d == "" {
		d = "cube.app"
	}
	return d
}

// ── Response transformers ───────────────────────────────────────────────────

// TransformSandboxList converts CubeMaster list response to frontend format.
func TransformSandboxList(raw json.RawMessage) interface{} {
	var env CMEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return nil
	}

	var items []CMSandboxListItem
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
		startedAt := RawToISO(item.StartedAt)
		if startedAt == "" && item.CreateAt > 0 {
			startedAt = NanosToISO(item.CreateAt)
		}
		entry := map[string]interface{}{
			"sandboxID":   item.SandboxID,
			"clientID":    item.HostID,
			"cpuCount":    fmt.Sprintf("%dm", item.CPUCount*1000), // int cores → K8s millicores string
			"memoryMB":    item.MemoryMB,
			"startedAt":   startedAt,
			"endAt":       RawToISO(item.EndAt),
			"state":       SandboxStateFromRaw(item.Status),
			"templateID":  item.TemplateID,
			"envdVersion": item.Annotations["cube.master.components.envd.version"],
			"domain":      SandboxDomain(),
		}
		if item.Labels != nil {
			entry["metadata"] = item.Labels
		}
		result = append(result, entry)
	}
	return result
}

// TransformSandboxDetail converts CubeMaster info response to frontend format.
func TransformSandboxDetail(raw json.RawMessage) interface{} {
	var env CMEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return json.RawMessage(raw)
	}
	if env.Ret != nil && env.Ret.RetCode != 0 && env.Ret.RetCode != 200 {
		return nil
	}

	var items []CMSandboxDetailItem
	if err := json.Unmarshal(env.Data, &items); err != nil || len(items) == 0 {
		return nil
	}

	item := items[0]
	// Find primary container (type == "sandbox" or container_id == sandbox_id).
	var primary *CMSandboxContainer
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
		memoryMB = ParseMemoryMB(primary.Mem)
		startedAt = NanosToISO(primary.CreateAt)
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
		"endAt":       RawToISO(item.EndAt),
		"state":       SandboxStateFromInt(item.Status),
		"templateID":  templateID,
		"envdVersion": item.Annotations["cube.master.components.envd.version"],
		"namespace":   item.Namespace,
		"hostID":      item.HostID,
		"domain":      SandboxDomain(),
	}
	if item.Labels != nil {
		result["metadata"] = item.Labels
	}
	return result
}

// TransformTemplateDetail converts CubeMaster's template response to the
// frontend format. Handles both the standard {ret, data} envelope and the
// flat single-template response shape.
func TransformTemplateDetail(raw json.RawMessage) interface{} {
	var env CMEnvelope
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
		camelKey := SnakeToCamel(k)
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

// TransformCreateTemplateRequest converts the frontend's create template
// request body to CubeMaster's CreateTemplateFromImageReq format.
// Matches the old CubeAPI create_template service transformation.
func TransformCreateTemplateRequest(body map[string]interface{}) map[string]interface{} {
	cmReq := map[string]interface{}{}

	// Direct field mappings (frontend camelCase → CubeMaster snake_case).
	cmReq["source_image_ref"] = strings.TrimSpace(GetString(body, "image"))
	cmReq["template_id"] = GetString(body, "templateID") // empty = auto-generate
	if v := GetString(body, "instanceType"); v != "" {
		cmReq["instance_type"] = v
	}
	if v := GetString(body, "writableLayerSize"); v != "" {
		cmReq["writable_layer_size"] = v
	}
	if v := GetArray(body, "exposedPorts"); len(v) > 0 {
		cmReq["exposed_ports"] = v
	}
	if v := GetString(body, "networkType"); v != "" {
		cmReq["network_type"] = v
	}
	if v := GetString(body, "registryUsername"); v != "" {
		cmReq["registry_username"] = v
	}
	if v := GetString(body, "registryPassword"); v != "" {
		cmReq["registry_password"] = v
	}
	if v := GetArray(body, "nodes"); len(v) > 0 {
		cmReq["distribution_scope"] = v
	}
	if v, ok := body["with_cube_ca"]; ok {
		cmReq["with_cube_ca"] = v
	}

	// Build container_overrides from cpu, memory, command, args, dns, probe, env.
	overrides := map[string]interface{}{}
	hasOverrides := false

	if cmd := GetArray(body, "command"); len(cmd) > 0 {
		overrides["command"] = cmd
		hasOverrides = true
	}
	if args := GetArray(body, "args"); len(args) > 0 {
		overrides["args"] = args
		hasOverrides = true
	}

	// Resources: cpu (int millicores) → "Nm", memory (int MB) → "NMi"
	resources := map[string]interface{}{}
	if cpu, ok := GetFloat(body, "cpu"); ok {
		resources["cpu"] = fmt.Sprintf("%dm", int(cpu))
		hasOverrides = true
	}
	if mem, ok := GetFloat(body, "memory"); ok {
		resources["mem"] = fmt.Sprintf("%dMi", int(mem))
		hasOverrides = true
	}
	if len(resources) > 0 {
		overrides["resources"] = resources
	}

	// Probe: probePort (or first exposedPort) + probePath → HTTP GET probe
	// Matches old CubeAPI build_template_probe defaults.
	probePort, hasProbePort := GetFloat(body, "probePort")
	if !hasProbePort {
		if ports := GetArray(body, "exposedPorts"); len(ports) > 0 {
			if p, ok := ports[0].(float64); ok {
				probePort = p
				hasProbePort = true
			}
		}
	}
	if hasProbePort {
		probePath := GetString(body, "probePath")
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
	if dns := GetArray(body, "dns"); len(dns) > 0 {
		overrides["dns_config"] = map[string]interface{}{
			"servers":  dns,
			"searches": []interface{}{},
		}
		hasOverrides = true
	}

	// Env: ["A=1"] → envs: [{key:"A", value:"1"}]
	if envStrs := GetArray(body, "env"); len(envStrs) > 0 {
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
	if v := GetArray(body, "allowOut"); len(v) > 0 {
		netCfg["allowOut"] = v
		hasNetCfg = true
	}
	if v := GetArray(body, "denyOut"); len(v) > 0 {
		netCfg["denyOut"] = v
		hasNetCfg = true
	}
	if hasNetCfg {
		cmReq["cube_network_config"] = netCfg
	}

	return cmReq
}

// ── typed map helpers ───────────────────────────────────────────────────────

// GetString returns the string value for key in m, or "" if missing / wrong type.
func GetString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetFloat returns the numeric value for key in m as float64.
func GetFloat(m map[string]interface{}, key string) (float64, bool) {
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

// GetArray returns the []interface{} value for key in m, or nil if missing.
func GetArray(m map[string]interface{}, key string) []interface{} {
	if v, ok := m[key]; ok {
		if arr, ok := v.([]interface{}); ok {
			return arr
		}
	}
	return nil
}

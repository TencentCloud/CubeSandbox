// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

// ConfigHandler handles runtime config HTTP requests.
type ConfigHandler struct {
	bind            string
	rateLimitPerSec uint32
	authEnabled     bool
	sandboxDomain   string
	instanceType    string
}

// NewConfigHandler creates a new config handler.
func NewConfigHandler(bind string, rateLimitPerSec uint32, authEnabled bool, sandboxDomain, instanceType string) *ConfigHandler {
	return &ConfigHandler{
		bind:            bind,
		rateLimitPerSec: rateLimitPerSec,
		authEnabled:     authEnabled,
		sandboxDomain:   sandboxDomain,
		instanceType:    instanceType,
	}
}

// RuntimeConfig is the response for GET /config.
type RuntimeConfig struct {
	APIEndpoint     string `json:"apiEndpoint"`    // CUBE_API_PUBLIC_HOST + /cubeapi/v1 (E2B SDK compatible, legacy)
	OpsAPIEndpoint  string `json:"opsApiEndpoint"` // CUBE_OPS_PUBLIC_HOST + /opsapi/v1 (CubeOps ops API)
	RateLimitPerSec uint32 `json:"rateLimitPerSec"`
	AuthEnabled     bool   `json:"authEnabled"`
	SandboxDomain   string `json:"sandboxDomain"`
	InstanceType    string `json:"instanceType"`
}

// Register installs the config routes on the given router group.
func (h *ConfigHandler) Register(r *gin.RouterGroup) {
	r.GET("/config", h.GetConfig)
}

// GetConfig handles GET /config.
func (h *ConfigHandler) GetConfig(c *gin.Context) {
	httputil.WriteJSON(c, http.StatusOK, RuntimeConfig{
		APIEndpoint:     publicAPIEndpoint(h.bind),
		OpsAPIEndpoint:  publicOpsAPIEndpoint(h.bind),
		RateLimitPerSec: h.rateLimitPerSec,
		AuthEnabled:     h.authEnabled,
		SandboxDomain:   h.sandboxDomain,
		InstanceType:    h.instanceType,
	})
}

// publicAPIEndpoint builds the public-facing SDK API endpoint URL (E2B compatible).
// Reads CUBE_API_PUBLIC_HOST; falls back to the bind address + /cubeapi/v1.
// This is the legacy CubeAPI-compatible entry point used by external SDK clients;
// nginx rewrites /cubeapi/v1/* to /api/v1/sdk/* before reaching CubeOps.
func publicAPIEndpoint(bind string) string {
	if v := os.Getenv("CUBE_API_PUBLIC_HOST"); v != "" {
		v = strings.TrimSpace(v)
		if v != "" {
			withScheme := v
			if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				withScheme = "http://" + v
			}
			base := strings.TrimRight(withScheme, "/")
			if strings.HasSuffix(base, "/cubeapi/v1") {
				return base
			}
			return base + "/cubeapi/v1"
		}
	}
	bindAddr := strings.ReplaceAll(bind, "0.0.0.0", "127.0.0.1")
	return "http://" + bindAddr + "/cubeapi/v1"
}

// publicOpsAPIEndpoint builds the public-facing CubeOps ops API endpoint URL.
// Reads CUBE_OPS_PUBLIC_HOST; falls back to the bind address + /opsapi/v1.
// This is the entry point the WebUI uses for all ops/* calls (cluster, agenthub, etc.).
func publicOpsAPIEndpoint(bind string) string {
	if v := os.Getenv("CUBE_OPS_PUBLIC_HOST"); v != "" {
		v = strings.TrimSpace(v)
		if v != "" {
			withScheme := v
			if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				withScheme = "http://" + v
			}
			base := strings.TrimRight(withScheme, "/")
			if strings.HasSuffix(base, "/opsapi/v1") {
				return base
			}
			return base + "/opsapi/v1"
		}
	}
	bindAddr := strings.ReplaceAll(bind, "0.0.0.0", "127.0.0.1")
	return "http://" + bindAddr + "/opsapi/v1"
}

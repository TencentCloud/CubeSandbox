// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"os"
	"strings"
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
	APIEndpoint     string `json:"apiEndpoint"`
	RateLimitPerSec uint32 `json:"rateLimitPerSec"`
	AuthEnabled     bool   `json:"authEnabled"`
	SandboxDomain   string `json:"sandboxDomain"`
	InstanceType    string `json:"instanceType"`
}

// GetConfig handles GET /config.
func (h *ConfigHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, RuntimeConfig{
		APIEndpoint:     publicAPIEndpoint(h.bind),
		RateLimitPerSec: h.rateLimitPerSec,
		AuthEnabled:     h.authEnabled,
		SandboxDomain:   h.sandboxDomain,
		InstanceType:    h.instanceType,
	})
}

// publicAPIEndpoint builds the public-facing API endpoint URL.
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

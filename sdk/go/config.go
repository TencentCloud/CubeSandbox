// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIURL         = "http://127.0.0.1:3000"
	defaultProxyPortHTTP  = 80
	defaultSandboxDomain  = "cube.app"
	defaultSandboxTimeout = 300 * time.Second
	defaultRequestTimeout = 30 * time.Second
)

// Config holds SDK configuration for control-plane and data-plane requests.
type Config struct {
	APIURL         string
	APIKey         string
	TemplateID     string
	ProxyNodeIP    string
	ProxyPortHTTP  int
	ProxyScheme    string
	SandboxDomain  string
	Timeout        time.Duration
	RequestTimeout time.Duration
}

// NewConfigFromEnv builds Config from environment variables.
//
// CUBE_API_URL and CUBE_API_KEY take precedence over E2B_API_URL and
// E2B_API_KEY for compatibility with existing E2B-style deployments.
func NewConfigFromEnv() Config {
	cfg := Config{
		APIURL:         firstEnv("CUBE_API_URL", "E2B_API_URL"),
		APIKey:         firstEnv("CUBE_API_KEY", "E2B_API_KEY"),
		TemplateID:     strings.TrimSpace(os.Getenv("CUBE_TEMPLATE_ID")),
		ProxyNodeIP:    strings.TrimSpace(os.Getenv("CUBE_PROXY_NODE_IP")),
		ProxyPortHTTP:  parseIntEnv("CUBE_PROXY_PORT_HTTP", defaultProxyPortHTTP),
		ProxyScheme:    strings.TrimSpace(os.Getenv("CUBE_PROXY_SCHEME")),
		SandboxDomain:  strings.TrimSpace(os.Getenv("CUBE_SANDBOX_DOMAIN")),
		Timeout:        parseDurationEnv("CUBE_TIMEOUT", defaultSandboxTimeout),
		RequestTimeout: parseDurationEnv("CUBE_REQUEST_TIMEOUT", defaultRequestTimeout),
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) Config {
	cfg.APIURL = strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if cfg.APIURL == "" {
		cfg.APIURL = defaultAPIURL
	}
	cfg.SandboxDomain = strings.TrimSpace(cfg.SandboxDomain)
	if cfg.SandboxDomain == "" {
		cfg.SandboxDomain = defaultSandboxDomain
	}
	if cfg.ProxyPortHTTP <= 0 {
		cfg.ProxyPortHTTP = defaultProxyPortHTTP
	}
	cfg.ProxyScheme = normalizeProxyScheme(cfg.ProxyScheme, cfg.ProxyPortHTTP)
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultSandboxTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return cfg
}

func normalizeProxyScheme(scheme string, port int) string {
	normalized := strings.ToLower(strings.TrimSpace(scheme))
	switch normalized {
	case "http", "https":
		return normalized
	}
	if port == 443 {
		return "https"
	}
	return "http"
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func parseIntEnv(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseDurationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds * float64(time.Second))
}

func durationSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	seconds := int(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return seconds
}

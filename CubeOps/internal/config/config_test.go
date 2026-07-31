// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoad_FromYAML proves that config.Load() reads values from the YAML
// file pointed to by CUBE_OPS_CONFIG. This is the test the reviewer's
// "use a config yaml is better" comment asked for — it demonstrates the
// YAML path is wired up, not just documented.
func TestLoad_FromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`bind: "0.0.0.0:9999"
log_level: "debug"
cubemaster_addr: "http://1.2.3.4:8089"
sandbox_domain: "test.example.com"
database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
jwt_secret: "yaml-secret"
access_ttl: "30m"
refresh_ttl: "336h"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "0.0.0.0:9999" {
		t.Errorf("Bind = %q, want 0.0.0.0:9999", cfg.Bind)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.CubeMasterAddr != "http://1.2.3.4:8089" {
		t.Errorf("CubeMasterAddr = %q, want http://1.2.3.4:8089", cfg.CubeMasterAddr)
	}
	if cfg.SandboxDomain != "test.example.com" {
		t.Errorf("SandboxDomain = %q, want test.example.com", cfg.SandboxDomain)
	}
	if cfg.JWTSecret != "yaml-secret" {
		t.Errorf("JWTSecret = %q, want yaml-secret", cfg.JWTSecret)
	}
}

// TestLoad_EnvOverridesYAML proves that environment variables take
// precedence over YAML values — the documented resolution order.
func TestLoad_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`bind: "0.0.0.0:9999"
database_url: "mysql://root:pass@127.0.0.1:3306/yamldb"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_OPS_BIND", "127.0.0.1:7777")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:7777" {
		t.Errorf("Bind = %q, want 127.0.0.1:7777 (env should override YAML)", cfg.Bind)
	}
}

// TestLoad_NoYAML_UsesEnvAndDefaults proves the system still works without
// a YAML file — existing deployments using only env vars are unaffected.
func TestLoad_NoYAML_UsesEnvAndDefaults(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/envdb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL != "mysql://root:pass@127.0.0.1:3306/envdb" {
		t.Errorf("DatabaseURL = %q, want envdb URL", cfg.DatabaseURL)
	}
	if cfg.Bind != "127.0.0.1:3010" {
		t.Errorf("Bind = %q, want default 127.0.0.1:3010", cfg.Bind)
	}
}

// TestLoad_MissingDB_Fails proves we still require a database URL.
func TestLoad_MissingDB_Fails(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "")
	// Also clear individual MySQL env vars so buildMySQLURL returns "".
	t.Setenv("CUBE_SANDBOX_MYSQL_HOST", "")

	_, err := Load()
	if err == nil {
		t.Error("Load with no DB config = nil err, want error")
	}
}

func TestLoad_WebhookFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
redis_url: "redis://127.0.0.1:6379/0"
webhook:
  enabled: true
  workers: 4
  endpoints:
    - name: receiver
      url: "http://127.0.0.1:9000/webhook"
      events: ["sandbox.created", "sandbox.deleted"]
      secret: "change-me"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Webhook.Enabled || cfg.Webhook.ConsumerGroup != "cubeops-webhook" {
		t.Fatalf("unexpected webhook config: %+v", cfg.Webhook)
	}
	if len(cfg.Webhook.Endpoints) != 1 || cfg.Webhook.Endpoints[0].MaxRetries == nil || *cfg.Webhook.Endpoints[0].MaxRetries != 3 {
		t.Fatalf("webhook endpoint defaults not applied: %+v", cfg.Webhook.Endpoints)
	}
}

func TestLoad_WebhookEndpointAllowsZeroRetries(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
redis_url: "redis://127.0.0.1:6379/0"
webhook:
  enabled: true
  endpoints:
    - name: receiver
      url: "http://127.0.0.1:9000/webhook"
      events: ["sandbox.created"]
      max_retries: 0
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.Endpoints[0].MaxRetries == nil || *cfg.Webhook.Endpoints[0].MaxRetries != 0 {
		t.Fatalf("endpoint max_retries = %v, want explicit zero", cfg.Webhook.Endpoints[0].MaxRetries)
	}
}

func TestLoad_WebhookRejectsNegativeRetries(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
	}{
		{
			name: "default",
			yamlContent: `database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
redis_url: "redis://127.0.0.1:6379/0"
webhook:
  enabled: true
  default_max_retries: -1
  endpoints:
    - name: receiver
      url: "http://127.0.0.1:9000/webhook"
      events: ["sandbox.created"]
`,
		},
		{
			name: "endpoint",
			yamlContent: `database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
redis_url: "redis://127.0.0.1:6379/0"
webhook:
  enabled: true
  endpoints:
    - name: receiver
      url: "http://127.0.0.1:9000/webhook"
      events: ["sandbox.created"]
      max_retries: -1
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			yamlPath := filepath.Join(dir, "ops.yaml")
			if err := os.WriteFile(yamlPath, []byte(test.yamlContent), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			t.Setenv("CUBE_OPS_CONFIG", yamlPath)

			if _, err := Load(); err == nil {
				t.Fatal("Load accepted negative webhook retries")
			}
		})
	}
}

func TestLoad_WebhookEndpointsFromEnvironment(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/testdb")
	t.Setenv("REDIS_URL", "redis://127.0.0.1:6379/0")
	t.Setenv("CUBE_OPS_WEBHOOK_ENABLED", "true")
	t.Setenv("CUBE_OPS_WEBHOOK_ENDPOINTS",
		`[{"name":"receiver","url":"http://127.0.0.1:9000/webhook","events":["sandbox.created"],"secret":"change-me"}]`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Webhook.Enabled || len(cfg.Webhook.Endpoints) != 1 {
		t.Fatalf("unexpected webhook config: %+v", cfg.Webhook)
	}
	if cfg.Webhook.Endpoints[0].Timeout != 3*time.Second {
		t.Fatalf("endpoint timeout = %s", cfg.Webhook.Endpoints[0].Timeout)
	}
}

func TestLoad_DisabledWebhookIgnoresInvalidEndpointsFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
redis_url: "redis://127.0.0.1:6379/0"
webhook:
  enabled: true
  endpoints:
    - name: receiver
      url: "http://127.0.0.1:9000/webhook"
      events: ["sandbox.created"]
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_OPS_WEBHOOK_ENABLED", "false")
	t.Setenv("CUBE_OPS_WEBHOOK_ENDPOINTS", "not-json")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.Enabled {
		t.Fatal("webhook remained enabled after environment override")
	}
}

func TestLoad_EnabledWebhookRejectsInvalidEndpointsFromEnvironment(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/testdb")
	t.Setenv("REDIS_URL", "redis://127.0.0.1:6379/0")
	t.Setenv("CUBE_OPS_WEBHOOK_ENABLED", "true")
	t.Setenv("CUBE_OPS_WEBHOOK_ENDPOINTS", "not-json")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CUBE_OPS_WEBHOOK_ENDPOINTS") {
		t.Fatalf("Load error = %v, want invalid endpoints error", err)
	}
}

func TestLoad_RejectsInvalidWebhookEnabledValue(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/testdb")
	t.Setenv("CUBE_OPS_WEBHOOK_ENABLED", "not-a-bool")
	t.Setenv("CUBE_OPS_WEBHOOK_ENDPOINTS", "not-json")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CUBE_OPS_WEBHOOK_ENABLED") {
		t.Fatalf("Load error = %v, want invalid enabled value error", err)
	}
}

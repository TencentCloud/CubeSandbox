// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_FromYAML proves that config.Load() reads values from the YAML
// file pointed to by CUBE_OPS_CONFIG. This is the test the reviewer's
// "use a config yaml is better" comment asked for — it demonstrates the
// YAML path is wired up, not just documented.
func TestLoad_FromYAML(t *testing.T) {
	unsetConfigTestEnv(t, "CUBE_TERMINAL_INTERNAL_TOKEN")
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	yamlContent := []byte(`bind: "0.0.0.0:9999"
log_level: "debug"
cubemaster_addr: "http://1.2.3.4:8089"
sandbox_domain: "test.example.com"
terminal:
  internal_token: "yaml-terminal-secret"
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
	if cfg.Terminal.InternalToken != "yaml-terminal-secret" {
		t.Errorf("Terminal.InternalToken mismatch")
	}
}

func unsetConfigTestEnv(t *testing.T, key string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			if err := os.Setenv(key, old); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("unset %s during cleanup: %v", key, err)
		}
	})
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
	t.Setenv("CUBE_TERMINAL_INTERNAL_TOKEN", "environment-terminal-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:7777" {
		t.Errorf("Bind = %q, want 127.0.0.1:7777 (env should override YAML)", cfg.Bind)
	}
	if cfg.Terminal.InternalToken != "environment-terminal-secret" {
		t.Errorf("Terminal.InternalToken did not use environment override")
	}
}

func TestLoad_EmptyTerminalTokenFailsClosed(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ops.yaml")
	if err := os.WriteFile(yamlPath, []byte("database_url: mysql://root:pass@127.0.0.1:3306/testdb\nterminal:\n  internal_token: yaml-terminal-secret\n"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_TERMINAL_INTERNAL_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Terminal.InternalToken != "" {
		t.Error("empty environment token must disable the terminal gateway")
	}
}

func TestLoad_ShortTerminalTokenFails(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/ops.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/envdb")
	t.Setenv("CUBE_TERMINAL_INTERNAL_TOKEN", "too-short")

	if _, err := Load(); err == nil {
		t.Error("Load with a short terminal token = nil err, want error")
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

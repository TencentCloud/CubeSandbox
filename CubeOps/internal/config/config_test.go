// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoad_FromYAML proves that config.Load() reads values from the YAML
// file pointed to by CUBE_OPS_CONFIG. This is the test the reviewer's
// "use a config yaml is better" comment asked for — it demonstrates the
// YAML path is wired up, not just documented.
func TestLoad_FromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := []byte(`bind: "0.0.0.0:9999"
log_level: "debug"
cubemaster_addr: "http://1.2.3.4:8089"
sandbox_domain: "test.example.com"
database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
jwt_secret: "yaml-secret"
access_ttl: "30m"
refresh_ttl: "336h"
redis_db: 7
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("REDIS_DB", "")

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
	if cfg.RedisDB != 7 {
		t.Errorf("RedisDB = %d, want 7", cfg.RedisDB)
	}
}

// TestLoad_EnvOverridesYAML proves that environment variables take
// precedence over YAML values — the documented resolution order.
func TestLoad_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := []byte(`bind: "0.0.0.0:9999"
database_url: "mysql://root:pass@127.0.0.1:3306/yamldb"
redis_db: 3
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_OPS_BIND", "127.0.0.1:7777")
	t.Setenv("REDIS_DB", "9")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Bind != "127.0.0.1:7777" {
		t.Errorf("Bind = %q, want 127.0.0.1:7777 (env should override YAML)", cfg.Bind)
	}
	if cfg.RedisDB != 9 {
		t.Errorf("RedisDB = %d, want 9 (env should override YAML)", cfg.RedisDB)
	}
}

// TestLoad_NoYAML_UsesEnvAndDefaults proves the system still works without
// a YAML file — existing deployments using only env vars are unaffected.
func TestLoad_NoYAML_UsesEnvAndDefaults(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/config.yaml")
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
	if cfg.Warehouse.WorkDir != "/var/tmp/cubeops-warehouse" {
		t.Errorf("Warehouse.WorkDir = %q, want default /var/tmp/cubeops-warehouse", cfg.Warehouse.WorkDir)
	}
}

// TestLoad_MissingDB_Fails proves we still require a database URL.
func TestLoad_MissingDB_Fails(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/config.yaml")
	t.Setenv("DATABASE_URL", "")
	// Also clear individual MySQL env vars so buildMySQLURL returns "".
	t.Setenv("CUBE_SANDBOX_MYSQL_HOST", "")

	_, err := Load()
	if err == nil {
		t.Error("Load with no DB config = nil err, want error")
	}
}

func TestLoad_WarehouseSection(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
s3:
  endpoint: "http://minio:9000"
  access_key_id: "ak"
  secret_access_key: "sk"
  bucket: "cube-ops"
warehouse:
  work_dir: "/tmp/wh"
  upload_timeout: "5m"
  fetch_timeout: "10m"
  presign_ttl: "2m"
  github_repos:
    - "acme/box"
  cnb_repos:
    - "acme/cnb"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Warehouse.WorkDir != "/tmp/wh" {
		t.Errorf("Warehouse.WorkDir = %q, want /tmp/wh", cfg.Warehouse.WorkDir)
	}
	if cfg.Warehouse.UploadTimeout != 5*time.Minute {
		t.Errorf("Warehouse.UploadTimeout = %v, want 5m", cfg.Warehouse.UploadTimeout)
	}
	if cfg.Warehouse.FetchTimeout != 10*time.Minute {
		t.Errorf("Warehouse.FetchTimeout = %v, want 10m", cfg.Warehouse.FetchTimeout)
	}
	if len(cfg.Warehouse.GitHubRepos) != 1 || cfg.Warehouse.GitHubRepos[0] != "acme/box" {
		t.Errorf("Warehouse.GitHubRepos = %v, want [acme/box]", cfg.Warehouse.GitHubRepos)
	}
	if cfg.S3.Endpoint != "http://minio:9000" {
		t.Errorf("S3.Endpoint = %q", cfg.S3.Endpoint)
	}
	if cfg.S3.Bucket != "cube-ops" {
		t.Errorf("S3.Bucket = %q, want cube-ops", cfg.S3.Bucket)
	}
	if cfg.Warehouse.PresignTTL != 2*time.Minute {
		t.Errorf("Warehouse.PresignTTL = %v, want 2m", cfg.Warehouse.PresignTTL)
	}
	if !cfg.S3Configured() {
		t.Error("S3Configured = false, want true")
	}
}

func TestLoad_S3EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
s3:
  endpoint: "http://yaml:9000"
  bucket: "from-yaml"
warehouse:
  presign_ttl: "3m"
`), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_OPS_S3_ENDPOINT", "http://env:9000")
	t.Setenv("CUBE_OPS_S3_ACCESS_KEY_ID", "env-ak")
	t.Setenv("CUBE_OPS_S3_SECRET_ACCESS_KEY", "env-sk")
	t.Setenv("CUBE_OPS_S3_BUCKET", "cube-ops")
	t.Setenv("CUBE_OPS_WAREHOUSE_PRESIGN_TTL", "7m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.Endpoint != "http://env:9000" {
		t.Errorf("S3.Endpoint = %q, want env", cfg.S3.Endpoint)
	}
	if cfg.S3.AccessKeyID != "env-ak" || cfg.S3.SecretAccessKey != "env-sk" {
		t.Errorf("S3 credentials not taken from env")
	}
	if cfg.S3.Bucket != "cube-ops" {
		t.Errorf("S3.Bucket = %q, want cube-ops", cfg.S3.Bucket)
	}
	if cfg.Warehouse.PresignTTL != 7*time.Minute {
		t.Errorf("Warehouse.PresignTTL = %v, want 7m", cfg.Warehouse.PresignTTL)
	}
	if !cfg.S3Configured() {
		t.Error("S3Configured = false, want true")
	}
}

func TestLoad_DefaultS3Bucket(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/config.yaml")
	t.Setenv("DATABASE_URL", "mysql://root:pass@127.0.0.1:3306/envdb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3.Bucket != "cube-ops" {
		t.Errorf("S3.Bucket = %q, want cube-ops", cfg.S3.Bucket)
	}
	if cfg.S3Configured() {
		t.Error("S3Configured = true, want false when endpoint/ak/sk empty")
	}
}

func TestLoad_WarehouseEnvOverridesSection(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	yamlContent := []byte(`database_url: "mysql://root:pass@127.0.0.1:3306/testdb"
warehouse:
  work_dir: "/tmp/wh"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	t.Setenv("CUBE_OPS_WAREHOUSE_WORK_DIR", "/env/wh")
	t.Setenv("CUBE_OPS_WAREHOUSE_GITHUB_REPOS", "env/one,env/two")
	t.Setenv("CUBE_OPS_WAREHOUSE_UPLOAD_TIMEOUT", "7m")
	t.Setenv("CUBE_OPS_WAREHOUSE_FETCH_TIMEOUT", "11m")
	t.Setenv("CUBE_OPS_WAREHOUSE_CNB_REPOS", "env/cnb")
	t.Setenv("CUBE_OPS_WAREHOUSE_GITHUB_TOKEN", "gh-secret")
	t.Setenv("CUBE_OPS_WAREHOUSE_CNB_TOKEN", "cnb-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Warehouse.WorkDir != "/env/wh" {
		t.Errorf("Warehouse.WorkDir = %q, want /env/wh (env should override YAML)", cfg.Warehouse.WorkDir)
	}
	if cfg.Warehouse.UploadTimeout != 7*time.Minute {
		t.Errorf("Warehouse.UploadTimeout = %v, want 7m", cfg.Warehouse.UploadTimeout)
	}
	if cfg.Warehouse.FetchTimeout != 11*time.Minute {
		t.Errorf("Warehouse.FetchTimeout = %v, want 11m", cfg.Warehouse.FetchTimeout)
	}
	if len(cfg.Warehouse.GitHubRepos) != 2 || cfg.Warehouse.GitHubRepos[0] != "env/one" || cfg.Warehouse.GitHubRepos[1] != "env/two" {
		t.Errorf("Warehouse.GitHubRepos = %v, want [env/one env/two]", cfg.Warehouse.GitHubRepos)
	}
	if len(cfg.Warehouse.CNBRepos) != 1 || cfg.Warehouse.CNBRepos[0] != "env/cnb" {
		t.Errorf("Warehouse.CNBRepos = %v, want [env/cnb]", cfg.Warehouse.CNBRepos)
	}
	if cfg.Warehouse.GitHubToken != "gh-secret" {
		t.Errorf("Warehouse.GitHubToken = %q, want gh-secret", cfg.Warehouse.GitHubToken)
	}
	if cfg.Warehouse.CNBToken != "cnb-secret" {
		t.Errorf("Warehouse.CNBToken = %q, want cnb-secret", cfg.Warehouse.CNBToken)
	}
}

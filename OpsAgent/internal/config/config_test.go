// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "node_id: node-a\nlisten_addr: \"10.0.0.1:8890\"\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeID != "node-a" || cfg.ListenAddr != "10.0.0.1:8890" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node_id: from-file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPS_AGENT_NODE_ID", "from-env")
	t.Setenv("OPS_AGENT_CUBEOPS_URL", "http://ops:3010")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeID != "from-env" || cfg.CubeOpsURL != "http://ops:3010" {
		t.Fatalf("env override not applied: %+v", cfg)
	}
}

func TestLoadEnvOnlyWithoutFile(t *testing.T) {
	t.Setenv("OPS_AGENT_NODE_ID", "node-env")
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("env-only load: %v", err)
	}
	if cfg.NodeID != "node-env" || cfg.ListenAddr == "" || cfg.DynamicconfPath == "" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestLoadFailsWithoutIdentity(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("missing file without env identity must fail")
	}
}

func TestLoadRejectsFileWithoutNodeID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen_addr: \"x:1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("file without node_id must fail")
	}
}

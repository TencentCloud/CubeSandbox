// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package config loads the ops-agent runtime configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the ops-agent runtime configuration.
type Config struct {
	// NodeID identifies this node to CubeOps (matches the registration id).
	NodeID string `yaml:"node_id"`
	// ListenAddr is the HTTP listen address; bind to a management interface
	// in multi-NIC deployments, never expose publicly.
	ListenAddr string `yaml:"listen_addr"`
	// CubeOpsURL is the control-plane base URL for pull reconcile.
	CubeOpsURL string `yaml:"cubeops_url"`
	// DynamicconfPath is the cubelet dynamic config file this agent manages.
	DynamicconfPath string `yaml:"dynamicconf_path"`
	// ReconcileInterval is the pull cadence.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// BackupKeep bounds the rotated dynamicconf backups.
	BackupKeep int `yaml:"backup_keep"`
	// SharedToken authenticates the CubeOps push; empty = reject all pushes.
	SharedToken string `yaml:"shared_token"`

	LogLevel string `yaml:"log_level"`
	LogDir   string `yaml:"log_dir"`
}

// Load reads the yaml file at path and applies defaults; a missing file
// is tolerated in env-only mode.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	case os.IsNotExist(err) && os.Getenv("OPS_AGENT_NODE_ID") != "":
		// env-only mode
	default:
		return nil, fmt.Errorf("read config: %w", err)
	}
	applyEnvOverrides(cfg)
	applyDefaults(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides lets container deployments inject the identity and
// endpoints without generating a config file: each env var wins over the
// yaml value when set.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("OPS_AGENT_NODE_ID"); v != "" {
		cfg.NodeID = v
	}
	if v := os.Getenv("OPS_AGENT_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("OPS_AGENT_CUBEOPS_URL"); v != "" {
		cfg.CubeOpsURL = v
	}
	if v := os.Getenv("OPS_AGENT_DYNAMICCONF_PATH"); v != "" {
		cfg.DynamicconfPath = v
	}
	if v := os.Getenv("OPS_AGENT_SHARED_TOKEN"); v != "" {
		cfg.SharedToken = v
	}
}

func applyDefaults(cfg *Config) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8890"
	}
	if cfg.CubeOpsURL == "" {
		cfg.CubeOpsURL = "http://127.0.0.1:3010"
	}
	if cfg.DynamicconfPath == "" {
		cfg.DynamicconfPath = "/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml"
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 300 * time.Second
	}
	if cfg.BackupKeep <= 0 {
		cfg.BackupKeep = 5
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "/data/log/ops-agent"
	}
}

func validate(cfg *Config) error {
	if cfg.NodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	return nil
}

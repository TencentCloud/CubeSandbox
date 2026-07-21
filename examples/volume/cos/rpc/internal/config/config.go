// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultVolumeBaseDir mirrors Cubelet's default parent directory, used when
// AttachRequest.VolumeBaseDir is empty (older Cubelet).
const DefaultVolumeBaseDir = "/data/volume"

// Config holds COS credentials and mount settings for the plugin.
type Config struct {
	SecretID   string
	SecretKey  string
	Bucket     string
	Region     string
	PasswdFile string
	SocketPath string
	Listen     string
}

// ListenAddr returns the gRPC listen address (unix or tcp).
func (c *Config) ListenAddr() string {
	if c.Listen != "" {
		return c.Listen
	}
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return "/run/cube-volume-cos-rpc.sock"
}

// DefaultPath is the default plugin config file location.
const DefaultPath = "/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf"

// Load reads KEY=VALUE lines from path (shell-style, no export).
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("CUBE_COS_CONFIG")
	}
	if path == "" {
		path = DefaultPath
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	cfg := &Config{
		PasswdFile: "/etc/cube/.passwd-cosfs",
		SocketPath: "/run/cube-volume-cos-rpc.sock",
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)

		switch key {
		case "SECRET_ID":
			cfg.SecretID = val
		case "SECRET_KEY":
			cfg.SecretKey = val
		case "BUCKET":
			cfg.Bucket = val
		case "REGION":
			cfg.Region = val
		case "PASSWD_FILE":
			if val != "" {
				cfg.PasswdFile = val
			}
		case "SOCKET":
			if val != "" {
				cfg.SocketPath = val
			}
		case "LISTEN":
			if val != "" {
				cfg.Listen = val
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	switch {
	case c.SecretID == "":
		return fmt.Errorf("config: SECRET_ID is empty")
	case c.SecretKey == "":
		return fmt.Errorf("config: SECRET_KEY is empty")
	case c.Bucket == "":
		return fmt.Errorf("config: BUCKET is empty")
	case c.Region == "":
		return fmt.Errorf("config: REGION is empty")
	default:
		return nil
	}
}

// VolumePrefix returns the COS key prefix for a volume (trailing slash).
func VolumePrefix(volumeID string) string {
	return "volumes/" + volumeID + "/"
}

// CosFSSubdir returns the path segment passed to cosfs (no trailing slash).
func CosFSSubdir(volumeID string) string {
	return "volumes/" + volumeID
}

// MountPointUnder returns the per-volume FUSE mount path under baseDir. The
// path MUST live inside baseDir so it satisfies Cubelet's host_path check.
// baseDir is AttachRequest.VolumeBaseDir; it falls back to DefaultVolumeBaseDir
// when empty (e.g. detach records from an older Cubelet).
func MountPointUnder(baseDir, volumeID string) string {
	if baseDir == "" {
		baseDir = DefaultVolumeBaseDir
	}
	return filepath.Join(baseDir, "cos-"+volumeID)
}

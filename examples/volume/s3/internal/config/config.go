// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package config loads the S3 Volume plugin configuration file.
//
// The file uses shell-style KEY=VALUE lines so a single volume-s3.conf can be
// shared with tooling that sources it, but this package parses it rather than
// executing it.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultVolumeBaseDir mirrors Cubelet's default parent directory, used when
// --volume-base-dir is empty (older Cubelet).
const DefaultVolumeBaseDir = "/data/cube-shared/volume"

// DefaultRegion is the SigV4 signing region used when REGION is unset.
const DefaultRegion = "us-east-1"

// DefaultLockDir holds the per-volume attach/detach lock files.
const DefaultLockDir = "/run/cube-volume-s3"

// ConfigFileName is looked up next to the plugin executable.
const ConfigFileName = "volume-s3.conf"

// Config holds S3 credentials and mount settings for the plugin.
type Config struct {
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Endpoint        string
	Region          string

	// S3FSExtraOpts are extra s3fs flags, whitespace-separated in the file.
	S3FSExtraOpts []string

	// PathStyle forces path-style addressing instead of virtual-hosted-style.
	PathStyle bool

	// PasswdFile is the s3fs credential file path.
	PasswdFile string

	// LockDir holds per-volume flock files.
	LockDir string
}

// Load reads KEY=VALUE lines from path. When path is empty it falls back to
// $CUBE_S3_CONFIG, then to volume-s3.conf next to the executable.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("CUBE_S3_CONFIG")
	}
	if path == "" {
		dir, err := executableDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, ConfigFileName)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	cfg := &Config{
		Region:  DefaultRegion,
		LockDir: DefaultLockDir,
	}

	// ADDRESSING_STYLE is read for compatibility with the previous shell
	// implementation; it is not documented in volume-s3.conf.example.
	var addressingStyle string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Strip an unquoted inline comment: volume-s3.conf values (keys,
		// endpoints, bucket names) never contain '#', so cutting at the first
		// '#' is safe and matches the shell `source` behaviour the old plugin
		// relied on. A quoted '#' inside a value is not supported — keep the
		// config simple.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		val = unquote(strings.TrimSpace(val))

		switch key {
		case "ACCESS_KEY_ID":
			cfg.AccessKeyID = val
		case "SECRET_ACCESS_KEY":
			cfg.SecretAccessKey = val
		case "BUCKET":
			cfg.Bucket = val
		case "ENDPOINT":
			cfg.Endpoint = val
		case "REGION":
			if val != "" {
				cfg.Region = val
			}
		case "S3FS_EXTRA_OPTS":
			cfg.S3FSExtraOpts = strings.Fields(val)
		case "ADDRESSING_STYLE":
			addressingStyle = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	if v := os.Getenv("CUBE_S3_LOCK_DIR"); v != "" {
		cfg.LockDir = v
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// MinIO and other path-style endpoints break virtual-hosted-style
	// addressing, as do bucket names containing dots. s3fs is told via
	// S3FS_EXTRA_OPTS; mirror that choice for the control-plane client so both
	// sides address the same way.
	cfg.PathStyle = addressingStyle == "path" || hasPathRequestStyle(cfg.S3FSExtraOpts)

	if v := os.Getenv("CUBE_S3_PASSWD_FILE"); v != "" {
		cfg.PasswdFile = v
	} else {
		cfg.PasswdFile = "/etc/cube/.passwd-s3fs-volume-" + cfg.Bucket
	}

	return cfg, nil
}

func (c *Config) validate() error {
	switch {
	case c.AccessKeyID == "":
		return fmt.Errorf("config: ACCESS_KEY_ID is empty")
	case c.SecretAccessKey == "":
		return fmt.Errorf("config: SECRET_ACCESS_KEY is empty")
	case c.Bucket == "":
		return fmt.Errorf("config: BUCKET is empty")
	case c.Endpoint == "":
		return fmt.Errorf("config: ENDPOINT is empty (set your S3-compatible endpoint URL)")
	default:
		return nil
	}
}

// hasPathRequestStyle reports whether s3fs was told to use path-style
// addressing (-ouse_path_request_style).
func hasPathRequestStyle(opts []string) bool {
	for _, o := range opts {
		if strings.Contains(o, "path_request_style") {
			return true
		}
	}
	return false
}

// unquote strips one layer of matching single or double quotes, so values
// written for `source` compatibility (S3FS_EXTRA_OPTS='-o...') parse the same.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// VolumePrefix returns the object key prefix for a volume (trailing slash).
// This is also the key of the directory object s3fs stats when mounting.
func VolumePrefix(volumeID string) string {
	return "volumes/" + volumeID + "/"
}

// VolumeSubdir returns the path segment passed to s3fs (no trailing slash).
func VolumeSubdir(volumeID string) string {
	return "volumes/" + volumeID
}

// MountPointUnder returns the per-volume FUSE mount path under baseDir. The
// path MUST live inside baseDir so it satisfies Cubelet's host_path check.
func MountPointUnder(baseDir, volumeID string) string {
	if baseDir == "" {
		baseDir = DefaultVolumeBaseDir
	}
	return filepath.Join(baseDir, "s3-"+volumeID)
}

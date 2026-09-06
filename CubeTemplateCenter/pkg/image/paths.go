// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultArtifactStoreDir  = "/data/CubeMaster/storage"
	fallbackArtifactStoreDir = "cubemaster-rootfs-artifacts-store"
)

func ArtifactWorkRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR")); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "cubemaster-rootfs-artifacts")
}

func ArtifactStoreRootDir() string {
	if value := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); value != "" {
		return value
	}
	return defaultArtifactStoreDir
}

func ext4FixedOverheadMiB() int64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_EXT4_FIXED_OVERHEAD_MIB")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 256
}

func ext4OverheadPercent() int64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_EXT4_OVERHEAD_PERCENT")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 1 && parsed <= 20 {
			return parsed
		}
	}
	return 10
}

func diskSpaceSafetyMargin() float64 {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_DISK_SPACE_SAFETY_MARGIN")); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed >= 1.0 {
			return parsed
		}
	}
	return 1.5
}

func loopMountExt4Enabled() bool {
	if v := strings.TrimSpace(os.Getenv("CUBEMASTER_LOOP_MOUNT_EXT4_ENABLED")); v != "" {
		enabled, err := strconv.ParseBool(v)
		return err == nil && enabled
	}
	return false
}

func ArtifactFallbackStoreRootDir() string {
	return filepath.Join(os.TempDir(), fallbackArtifactStoreDir)
}

func artifactStoreDir(artifactID string) string {
	return filepath.Join(ArtifactStoreRootDir(), artifactID)
}

func ResolveArtifactStoreDir(ctx context.Context, artifactID string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")); configured != "" {
		dir := filepath.Join(configured, artifactID)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("prepare configured artifact store root %s failed: %w", configured, err)
		}
		return dir, nil
	}
	primaryDir := artifactStoreDir(artifactID)
	if err := os.MkdirAll(filepath.Dir(primaryDir), 0o755); err == nil {
		return primaryDir, nil
	} else {
		fallbackDir := filepath.Join(ArtifactFallbackStoreRootDir(), artifactID)
		if fallbackErr := os.MkdirAll(filepath.Dir(fallbackDir), 0o755); fallbackErr == nil {
			log.G(ctx).Warnf("artifact store root %s is unavailable, fallback to %s: %v", ArtifactStoreRootDir(), ArtifactFallbackStoreRootDir(), err)
			return fallbackDir, nil
		} else {
			return "", fmt.Errorf("prepare artifact store root %s failed: %w; fallback %s failed: %v", ArtifactStoreRootDir(), err, ArtifactFallbackStoreRootDir(), fallbackErr)
		}
	}
}

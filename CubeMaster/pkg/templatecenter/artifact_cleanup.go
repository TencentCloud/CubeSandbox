// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"os"
	"path/filepath"
	"strings"
)

var deleteRootfsArtifactRecord = func(ctx context.Context, artifactID string) error {
	return store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).Delete(&models.RootfsArtifact{}).Error
}

func cleanupFailedRootfsArtifact(ctx context.Context, artifact *models.RootfsArtifact, instanceType string) error {
	if artifact == nil {
		return nil
	}
	var cleanupErr error
	if err := cleanupDistributedArtifact(ctx, artifact.ArtifactID, instanceType); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := cleanupLocalRootfsArtifact(artifact.ArtifactID, artifact.Ext4Path); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr == nil {
		if err := deleteRootfsArtifactRecord(ctx, artifact.ArtifactID); err == nil {
			return nil
		} else {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	updateErr := updateRootfsArtifact(ctx, artifact.ArtifactID, map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": fmt.Sprintf("artifact cleanup incomplete: %v", cleanupErr),
	})
	return errors.Join(cleanupErr, updateErr)
}

func cleanupLocalRootfsArtifact(artifactID, ext4Path string) error {
	if ext4Path == "" {
		return nil
	}
	if dir, ok := managedArtifactDir(artifactID, ext4Path); ok {
		return os.RemoveAll(dir) // NOCC:Path Traversal()
	}
	if err := os.Remove(ext4Path); err != nil && !errors.Is(err, os.ErrNotExist) { // NOCC:Path Traversal()
		return err
	}
	return nil
}

func managedArtifactDir(artifactID, ext4Path string) (string, bool) {
	if strings.TrimSpace(artifactID) == "" || strings.TrimSpace(ext4Path) == "" {
		return "", false
	}
	dir := filepath.Clean(filepath.Dir(ext4Path))
	if filepath.Base(dir) != artifactID {
		return "", false
	}
	roots := []string{image.ArtifactWorkRootDir(), image.ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, image.ArtifactFallbackStoreRootDir())
	}
	for _, root := range roots {
		rel, err := filepath.Rel(filepath.Clean(root), dir)
		if err != nil {
			continue
		}
		if rel == artifactID {
			return dir, true
		}
	}
	return "", false
}

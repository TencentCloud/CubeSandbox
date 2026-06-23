// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"gorm.io/gorm"
)

var deleteRootfsArtifactRecord = func(ctx context.Context, artifactID string) error {
	// Placement rows and the artifact row must vanish together: a half-applied
	// delete that drops the artifact row but keeps placement (or vice versa)
	// would either orphan placement rows or strand the node list needed to
	// reclaim files. Wrap both in one transaction.
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(constants.ArtifactNodePlacementTableName).
			Where("artifact_id = ?", artifactID).Delete(&models.ArtifactNodePlacement{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).Delete(&models.RootfsArtifact{}).Error
	})
}

// cleanupFailedRootfsArtifact reclaims a freshly built artifact whose template
// creation failed. It routes through the shared last-owner cleanup so a
// concurrent build that reused the same fingerprint (its own active job/replica
// keeps the reference count > 0) is never deleted out from under. The build's
// own template is excluded so its in-flight job/definition do not pin the
// artifact and block its own failure cleanup.
func cleanupFailedRootfsArtifact(ctx context.Context, artifact *models.RootfsArtifact, instanceType, templateID string) error {
	if artifact == nil {
		return nil
	}
	return cleanupArtifactFully(ctx, artifact.ArtifactID, instanceType, templateID)
}

func cleanupLocalRootfsArtifact(artifactID, ext4Path string) error {
	if ext4Path == "" {
		return nil
	}
	// S8/L8: only ever RemoveAll inside a recognised managed artifact root. A
	// path outside the managed roots is NEVER deleted (no os.Remove fallback) —
	// it indicates corrupt metadata or an attempted out-of-bounds delete and is
	// surfaced as an error for manual handling rather than risking deletion of
	// an arbitrary host file.
	if dir, ok := managedArtifactDir(artifactID, ext4Path); ok {
		return os.RemoveAll(dir) // NOCC:Path Traversal()
	}
	log.G(context.Background()).Errorf(
		"refusing to delete rootfs artifact %s: ext4 path %q is outside managed artifact roots; manual cleanup required",
		artifactID, ext4Path)
	return fmt.Errorf("rootfs artifact %s ext4 path %q is outside managed artifact roots", artifactID, ext4Path)
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

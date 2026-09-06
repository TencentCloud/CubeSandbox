// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// This file implements artifact deletion for CubeTemplateCenter.
//
// Boundary: CubeMaster NEVER touches the filesystem or S3. When a template is
// deleted, CubeMaster only removes its own rows (template_definitions,
// template_replicas, image_jobs) and marks the artifact row CLEANUP_PENDING.
// CubeTemplateCenter owns the actual data removal:
//   - the ext4 object in S3/MinIO (if the artifact was uploaded)
//   - the ext4 file on local/shared disk (if present)
//   - the artifact row itself (after the data is gone)
//
// Deletion is triggered two ways (belt and suspenders):
//   1. Immediately, via POST /tc/api/v1/artifact/delete from CubeMaster.
//   2. Lazily, by the reconciler sweeping CLEANUP_PENDING rows whose updated_at
//      is older than a grace period (covers "Master crashed before notifying").
package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"gorm.io/gorm"
)

// ArtifactDeleter removes artifact data (S3 object + local ext4) and the
// artifact row. One instance per TC process; safe for concurrent use.
type ArtifactDeleter struct {
	db       *gorm.DB
	s3Client *s3store.Client // nil when S3 is not configured
}

// NewArtifactDeleter builds a deleter. s3Client may be nil (S3 disabled); in
// that case only local files are removed and S3 URLs on the row are ignored
// (they will simply age out via the bucket lifecycle, if any).
func NewArtifactDeleter(db *gorm.DB, s3Client *s3store.Client) *ArtifactDeleter {
	return &ArtifactDeleter{db: db, s3Client: s3Client}
}

// artifactTableName mirrors the constant used elsewhere in TC. Kept as a
// package-level var so tests can stub it if needed.
var artifactTableName = constants.RootfsArtifactTableName

// Delete removes the artifact identified by artifactID.
//
// Idempotent: deleting an already-deleted artifact returns nil. Safe against
// concurrent deletes: the row status transition to DELETING acts as the
// claim; a second caller sees status != CLEANUP_PENDING and returns nil.
//
// Failure handling: individual step failures (S3, local file) are logged but
// do NOT abort the row deletion — leaving the row behind would block template
// re-creation forever (fingerprint reuse check would keep finding it), which
// is strictly worse than leaking an orphaned S3 object. Orphaned objects are
// reclaimed by bucket lifecycle rules.
func (d *ArtifactDeleter) Delete(ctx context.Context, artifactID string) error {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return fmt.Errorf("artifact_id is required")
	}
	if d.db == nil {
		return fmt.Errorf("artifact deleter has no db handle")
	}
	logger := log.G(ctx).WithFields(map[string]any{"artifact_id": artifactID, "component": "artifact_delete"})

	// Claim the row: flip CLEANUP_PENDING -> DELETING atomically. If no row
	// was updated, either it does not exist or someone else is deleting it.
	tx := d.db.WithContext(ctx).Table(artifactTableName).
		Where("artifact_id = ?", artifactID).
		Where("status IN ?", []string{"CLEANUP_PENDING", "DELETING", "FAILED", "ORPHANED"}).
		Updates(map[string]any{"status": "DELETING"})
	if tx.Error != nil {
		return fmt.Errorf("claim artifact row: %w", tx.Error)
	}
	if tx.RowsAffected == 0 {
		// Already gone or being deleted by a peer replica — treat as success.
		logger.Infof("artifact already deleted or being deleted, skipping")
		return nil
	}

	// Load the row (for S3 URL + ext4 path) after claiming.
	var artifact models.RootfsArtifact
	if err := d.db.WithContext(ctx).Table(artifactTableName).
		Where("artifact_id = ?", artifactID).First(&artifact).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.Infof("artifact row vanished after claim, nothing to delete")
			return nil
		}
		return fmt.Errorf("load artifact row: %w", err)
	}

	// Delete the S3 object when the artifact was uploaded.
	if d.s3Client != nil && artifact.ArtifactURL != "" {
		if err := d.s3Client.Delete(ctx, artifact.ArtifactID); err != nil {
			logger.Warnf("delete s3 object fail (row will still be removed): %v", err)
		} else {
			logger.Infof("deleted s3 object for artifact")
		}
	}

	// Delete the local/shared ext4 file. Only when it sits inside a managed
	// artifact root (path-traversal guard).
	if artifact.Ext4Path != "" {
		if err := deleteLocalExt4(artifact.Ext4Path); err != nil {
			logger.Warnf("delete local ext4 fail (row will still be removed): %v", err)
		} else {
			logger.Infof("deleted local ext4 %s", artifact.Ext4Path)
		}
	}

	// Finally remove the row. Hard delete: the row carries no history worth
	// keeping, and soft-deleting would keep the fingerprint reuse check alive.
	if err := d.db.WithContext(ctx).Table(artifactTableName).
		Where("artifact_id = ?", artifactID).
		Delete(nil).Error; err != nil {
		return fmt.Errorf("delete artifact row: %w", err)
	}
	logger.Infof("artifact deleted")
	return nil
}

// deleteLocalExt4 removes the ext4 file's parent directory (the whole
// artifact dir) after verifying the path is inside a managed artifact root.
// The root check mirrors Master's validateExt4PathWithinStore so a tampered
// ext4_path in the DB cannot be used to delete arbitrary files.
func deleteLocalExt4(ext4Path string) error {
	abs, err := filepath.Abs(ext4Path)
	if err != nil {
		return fmt.Errorf("resolve ext4 path: %w", err)
	}
	dir := filepath.Dir(abs)
	if !managedArtifactDir(dir) {
		return fmt.Errorf("ext4 path %q is outside managed artifact roots; refusing to delete", ext4Path)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove artifact dir %s: %w", dir, err)
	}
	return nil
}

// managedArtifactDir reports whether dir is inside one of the artifact roots
// TC writes to: the work dir, the store dir, or the fallback store dir.
func managedArtifactDir(dir string) bool {
	roots := []string{image.ArtifactWorkRootDir(), image.ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, image.ArtifactFallbackStoreRootDir())
	}
	for _, root := range roots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if dir == rootAbs || strings.HasPrefix(dir, rootAbs+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

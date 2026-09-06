// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// artifactBuildLocks serializes ensureRootfsArtifact callers that target the
// same artifactID. Without this, two templates submitted in quick succession
// for the same image spec race on buildRootfsArtifact: both goroutines share
// the same workDir/storeDir/ext4Path, and one goroutine's defer cleanup can
// wipe the ext4 file while the other is still relying on it — the surviving
// caller then reaches distributeRootfsArtifact with a partial record
// (ext4_size_bytes=0, download_token=""), cubelet rejects the pull with
// "invalid size:0", and the template is marked FAILED.
//
// The lock is keyed by artifactID (deterministic from image+spec fingerprint)
// so only racing submits for the same image spec are serialized; different
// images build in parallel as before. DB claimRootfsArtifactForBuild only
// covers a short FOR UPDATE transaction and does not protect the filesystem
// build in image.BuildExt4.
// claimRootfsArtifactForBuild atomically ensures the artifact row exists and is
// marked BUILDING while holding its FOR UPDATE lock. It resurrects a
// soft-deleted or CLEANUP_PENDING row (raced with a concurrent
// last-owner-cleanup) instead of letting the build proceed against a row that
// is about to vanish. Because the deleter takes the same row lock in both its
// decision (TX1) and finalisation (TX2) phases, after this commit the deleter's
// phase-3 re-check observes a live BUILDING row plus the active build job and
// backs off without deleting or overwriting the in-flight build status.
//
// Master uses this when registering an artifact reported by TC in the BUILT
// callback (remote_build_resume.go).
func claimRootfsArtifactForBuild(ctx context.Context, artifactID, fingerprint string, req *types.CreateTemplateFromImageReq, sourceDigest string) (*models.RootfsArtifact, error) {
	var claimed *models.RootfsArtifact
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.RootfsArtifact
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Unscoped().
			Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).First(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			row := &models.RootfsArtifact{
				ArtifactID:              artifactID,
				TemplateSpecFingerprint: fingerprint,
				SourceImageRef:          req.SourceImageRef,
				SourceImageDigest:       sourceDigest,
				WritableLayerSize:       req.WritableLayerSize,
				Status:                  ArtifactStatusBuilding,
			}
			if createErr := tx.Table(constants.RootfsArtifactTableName).Create(row).Error; createErr != nil {
				return createErr
			}
			claimed = row
			return nil
		case err != nil:
			return err
		default:
			if updErr := tx.Unscoped().Table(constants.RootfsArtifactTableName).
				Where("artifact_id = ?", artifactID).
				Updates(map[string]any{
					"template_spec_fingerprint": fingerprint,
					"source_image_ref":          req.SourceImageRef,
					"source_image_digest":       sourceDigest,
					"writable_layer_size":       req.WritableLayerSize,
					"status":                    ArtifactStatusBuilding,
					"last_error":                "",
					"deleted_at":                nil,
					"updated_at":                time.Now(),
				}).Error; updErr != nil {
				return updErr
			}
			existing.Status = ArtifactStatusBuilding
			existing.DeletedAt = gorm.DeletedAt{}
			claimed = &existing
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func findReusableRootfsArtifact(ctx context.Context, fingerprint, artifactID string) (*models.RootfsArtifact, bool, error) {
	record, err := getRootfsArtifactByFingerprint(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByFingerprintUnscoped(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		record, err = getRootfsArtifactByIDUnscoped(ctx, artifactID)
		if err != nil {
			return nil, false, err
		}
	}
	record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
	return record, rootfsArtifactSoftDeleted(record), err
}

func validateReusableRootfsArtifact(record *models.RootfsArtifact, fingerprint, artifactID string) (*models.RootfsArtifact, error) {
	if record == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if record.ArtifactID != artifactID {
		return nil, fmt.Errorf("rootfs artifact id mismatch: want %s got %s", artifactID, record.ArtifactID)
	}
	if record.TemplateSpecFingerprint != "" && record.TemplateSpecFingerprint != fingerprint {
		return nil, fmt.Errorf("rootfs artifact %s fingerprint mismatch: want %s got %s", artifactID, fingerprint, record.TemplateSpecFingerprint)
	}
	return record, nil
}

func validateReusableRootfsArtifactFile(record *models.RootfsArtifact) error {
	if record == nil {
		return gorm.ErrRecordNotFound
	}
	if strings.TrimSpace(record.Ext4Path) == "" {
		return fmt.Errorf("rootfs artifact %s ext4 path is empty", record.ArtifactID)
	}
	if record.Ext4SizeBytes <= 0 {
		return fmt.Errorf("rootfs artifact %s ext4 size is invalid: %d", record.ArtifactID, record.Ext4SizeBytes)
	}
	info, err := os.Stat(record.Ext4Path) // NOCC:Path Traversal()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rootfs artifact %s ext4 file %q is missing: %w", record.ArtifactID, record.Ext4Path, err)
		}
		return fmt.Errorf("stat rootfs artifact %s ext4 file %q: %w", record.ArtifactID, record.Ext4Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rootfs artifact %s ext4 path %q is not a regular file", record.ArtifactID, record.Ext4Path)
	}
	if info.Size() != record.Ext4SizeBytes {
		return fmt.Errorf("rootfs artifact %s ext4 size mismatch: got %d want %d", record.ArtifactID, info.Size(), record.Ext4SizeBytes)
	}
	return nil
}

func rootfsArtifactSoftDeleted(record *models.RootfsArtifact) bool {
	return record != nil && record.DeletedAt.Valid
}

func restoreRootfsArtifact(ctx context.Context, artifactID string) error {
	tx := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).
		Updates(map[string]any{
			"deleted_at": nil,
			"updated_at": time.Now(),
		})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

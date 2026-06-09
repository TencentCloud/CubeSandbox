// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"gorm.io/gorm"
	"strings"
	"time"
)

func ensureRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
	var generatedReq *types.CreateCubeSandboxReq
	fingerprint := buildTemplateSpecFingerprint(req, source.Digest)
	artifactID := buildArtifactID(fingerprint)
	record, wasDeleted, err := findReusableRootfsArtifact(ctx, fingerprint, artifactID)
	if err == nil && wasDeleted {
		if restoreErr := restoreRootfsArtifact(ctx, artifactID); restoreErr != nil {
			return nil, nil, false, restoreErr
		}
		record.DeletedAt = gorm.DeletedAt{}
	}
	if err == nil && record.Status == ArtifactStatusReady && record.GeneratedRequestJSON != "" {
		generatedReq, err = generateTemplateCreateRequest(req, record, source.Config, downloadBaseURL)
		if err == nil {
			return record, generatedReq, false, nil
		}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, false, err
	}
	if record == nil {
		record = &models.RootfsArtifact{
			ArtifactID:              artifactID,
			TemplateSpecFingerprint: fingerprint,
			SourceImageRef:          req.SourceImageRef,
			SourceImageDigest:       source.Digest,
			WritableLayerSize:       req.WritableLayerSize,
			Status:                  ArtifactStatusPending,
		}
		if createErr := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).Create(record).Error; createErr != nil {
			if !errors.Is(createErr, gorm.ErrDuplicatedKey) &&
				!strings.Contains(createErr.Error(), "1062") &&
				!strings.Contains(createErr.Error(), "Duplicate entry") {
				return nil, nil, false, createErr
			}
			record, wasDeleted, err = findReusableRootfsArtifact(ctx, fingerprint, artifactID)
			if err != nil {
				return nil, nil, false, createErr
			}
			if wasDeleted {
				if restoreErr := restoreRootfsArtifact(ctx, artifactID); restoreErr != nil {
					return nil, nil, false, restoreErr
				}
				record.DeletedAt = gorm.DeletedAt{}
			}
			if record.Status == ArtifactStatusReady && record.GeneratedRequestJSON != "" {
				generatedReq, err = generateTemplateCreateRequest(req, record, source.Config, downloadBaseURL)
				if err == nil {
					return record, generatedReq, false, nil
				}
			}
		}
	}
	_ = updateRootfsArtifact(ctx, artifactID, map[string]any{
		"template_spec_fingerprint": fingerprint,
		"source_image_ref":          req.SourceImageRef,
		"source_image_digest":       source.Digest,
		"writable_layer_size":       req.WritableLayerSize,
		"status":                    ArtifactStatusBuilding,
		"last_error":                "",
	})
	record, generatedReq, err = buildRootfsArtifact(ctx, record, req, source, downloadBaseURL)
	if err != nil {
		_ = updateRootfsArtifact(ctx, artifactID, map[string]any{
			"status":     ArtifactStatusFailed,
			"last_error": err.Error(),
		})
		return nil, nil, false, err
	}
	return record, generatedReq, true, nil
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

func buildRootfsArtifact(ctx context.Context, record *models.RootfsArtifact, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	result, err := image.BuildExt4(ctx, source, image.BuildOptions{ArtifactID: record.ArtifactID})
	if err != nil {
		return nil, nil, err
	}
	return finalizeArtifact(ctx, record, source, result.Ext4Path, result.SHA256, result.SizeBytes, downloadBaseURL, req)
}

// finalizeArtifact populates the artifact record with computed values, persists it,
// and returns the latest version.
func finalizeArtifact(ctx context.Context, record *models.RootfsArtifact, source *image.PreparedSource, ext4Path, shaValue string, sizeBytes int64, downloadBaseURL string, req *types.CreateTemplateFromImageReq) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	downloadToken := uuid.New().String()
	record.SourceImageDigest = source.Digest
	record.MasterNodeIP = source.MasterNodeIP
	record.Ext4Path = ext4Path
	record.Ext4SHA256 = shaValue
	record.Ext4SizeBytes = sizeBytes
	record.ImageConfigJSON = source.ConfigJSON
	record.DownloadToken = downloadToken
	record.Status = ArtifactStatusReady
	record.GCDeadline = time.Now().Add(defaultTemplateArtifactTTL).Unix()

	generatedReq, err := generateTemplateCreateRequest(req, record, source.Config, downloadBaseURL)
	if err != nil {
		return nil, nil, err
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		return nil, nil, err
	}
	record.GeneratedRequestJSON = string(reqPayload)
	if err := updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
		"source_image_digest":    record.SourceImageDigest,
		"master_node_ip":         record.MasterNodeIP,
		"ext4_path":              record.Ext4Path,
		"ext4_sha256":            record.Ext4SHA256,
		"ext4_size_bytes":        record.Ext4SizeBytes,
		"image_config_json":      record.ImageConfigJSON,
		"generated_request_json": record.GeneratedRequestJSON,
		"download_token":         record.DownloadToken,
		"status":                 record.Status,
		"gc_deadline":            record.GCDeadline,
		"last_error":             "",
	}); err != nil {
		return nil, nil, err
	}
	latest, err := getRootfsArtifactByID(ctx, record.ArtifactID)
	if err != nil {
		return nil, nil, err
	}
	return latest, generatedReq, nil
}

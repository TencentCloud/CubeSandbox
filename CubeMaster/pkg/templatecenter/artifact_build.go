// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
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
var artifactBuildLocks sync.Map // map[string]*sync.Mutex

func ensureRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
	var generatedReq *types.CreateCubeSandboxReq
	withCubeCA := resolveWithCubeCA(req.WithCubeCA)
	caPEM, caFingerprint, err := loadCubeEgressCA(ctx, withCubeCA)
	if err != nil {
		return nil, nil, false, err
	}
	fingerprint := buildTemplateSpecFingerprintWithCA(req, source.Digest, caFingerprint)
	artifactID := buildArtifactID(fingerprint)
	// Serialize concurrent builds of the same artifactID. Without this, two
	// submits of the same image spec race on workDir/storeDir/ext4Path; the
	// losing goroutine's defer cleanup can wipe the ext4 file while the
	// winner is still relying on it. See artifactBuildLocks comment for the
	// full failure mode.
	muV, _ := artifactBuildLocks.LoadOrStore(artifactID, &sync.Mutex{})
	mu := muV.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
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
	// Claim the artifact row under its FOR UPDATE lock before (re)building. This
	// serialises against last-owner-cleanup (which locks the same row in its
	// phases) and resurrects a CLEANUP_PENDING / soft-deleted row, so a
	// concurrent deletion cannot remove the artifact out from under this build
	// (FIX-1 create/reuse guard).
	claimed, claimErr := claimRootfsArtifactForBuild(ctx, artifactID, fingerprint, req, source.Digest)
	if claimErr != nil {
		return nil, nil, false, claimErr
	}
	if claimed != nil {
		record = claimed
	}
	record, generatedReq, err = buildRootfsArtifact(ctx, record, req, source, downloadBaseURL, caPEM, caFingerprint)
	if err != nil {
		_ = updateRootfsArtifact(ctx, artifactID, map[string]any{
			"status":     ArtifactStatusFailed,
			"last_error": err.Error(),
		})
		return nil, nil, false, err
	}
	return record, generatedReq, true, nil
}

// claimRootfsArtifactForBuild atomically ensures the artifact row exists and is
// marked BUILDING while holding its FOR UPDATE lock. It resurrects a
// soft-deleted or CLEANUP_PENDING row (raced with a concurrent
// last-owner-cleanup) instead of letting the build proceed against a row that
// is about to vanish. Because the deleter takes the same row lock in both its
// decision (TX1) and finalisation (TX2) phases, after this commit the deleter's
// phase-3 re-check observes a live BUILDING row plus the active build job and
// backs off without deleting or overwriting the in-flight build status.
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

func buildRootfsArtifact(ctx context.Context, record *models.RootfsArtifact, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string, caPEM []byte, caFingerprint string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	var caBakeResult cube_egress_ca.Result
	opts := image.BuildOptions{ArtifactID: record.ArtifactID}
	// Bake the CubeEgress root CA into the rootfs while it's still a mutable
	// host-side directory, before mkfs.ext4 freezes the layout. See
	// design/cube-egress-ca-bake.md for the contract.
	opts.PostRootfsExport = func(ctx context.Context, rootfsDir string) error {
		var err error
		caBakeResult, err = applyCubeEgressCAToRootfs(ctx, rootfsDir, caPEM, caFingerprint)
		return err
	}
	result, err := image.BuildExt4(ctx, source, opts)
	if err != nil {
		return nil, nil, err
	}
	return finalizeArtifact(ctx, record, source, result.Ext4Path, result.SHA256, result.SizeBytes, downloadBaseURL, req, caBakeResult)
}

// finalizeArtifact populates the artifact record with computed values, persists it,
// and returns the latest version.
func finalizeArtifact(ctx context.Context, record *models.RootfsArtifact, source *image.PreparedSource, ext4Path, shaValue string, sizeBytes int64, downloadBaseURL string, req *types.CreateTemplateFromImageReq, caBakeResult cube_egress_ca.Result) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
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
	record.CubeEgressCABaked = caBakeResult.Baked
	record.CubeEgressCAFingerprint = caBakeResult.Fingerprint
	record.CubeEgressCATargetsWritten = caBakeResult.TargetsWritten

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
		"source_image_digest":            record.SourceImageDigest,
		"master_node_ip":                 record.MasterNodeIP,
		"ext4_path":                      record.Ext4Path,
		"ext4_sha256":                    record.Ext4SHA256,
		"ext4_size_bytes":                record.Ext4SizeBytes,
		"image_config_json":              record.ImageConfigJSON,
		"generated_request_json":         record.GeneratedRequestJSON,
		"download_token":                 record.DownloadToken,
		"status":                         record.Status,
		"gc_deadline":                    record.GCDeadline,
		"last_error":                     "",
		"cube_egress_ca_baked":           record.CubeEgressCABaked,
		"cube_egress_ca_fingerprint":     record.CubeEgressCAFingerprint,
		"cube_egress_ca_targets_written": record.CubeEgressCATargetsWritten,
	}); err != nil {
		return nil, nil, err
	}
	latest, err := getRootfsArtifactByID(ctx, record.ArtifactID)
	if err != nil {
		return nil, nil, err
	}
	return latest, generatedReq, nil
}

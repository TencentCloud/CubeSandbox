// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/tcclient"
)

// TemplateArtifactMigrationResult is the outcome of migrating one template's
// rootfs artifact from CubeMaster local disk to TC-backed S3/local storage.
type TemplateArtifactMigrationResult struct {
	TemplateID string
	ArtifactID string
	Migrated   bool
	Cleaned    bool
}

type templateCenterUploadResult struct {
	Ext4Path   string
	Ext4SHA256 string
	SizeBytes  int64
}

// uploadArtifactFileToTC uploads one artifact ext4 file into CubeTemplateCenter's
// own artifact store and returns the stored file metadata.
var uploadArtifactFileToTC = func(ctx context.Context, artifactID, filePath string) (*templateCenterUploadResult, error) {
	endpoint := ""
	if cfg := config.GetConfig(); cfg != nil {
		endpoint = cfg.TemplateCenterAddr()
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("template center endpoint is not configured")
	}
	res, err := tcclient.NewClient(endpoint).UploadArtifact(ctx, artifactID, filePath)
	if err != nil {
		return nil, err
	}
	return &templateCenterUploadResult{
		Ext4Path:   res.Ext4Path,
		Ext4SHA256: res.Ext4SHA256,
		SizeBytes:  res.Ext4SizeBytes,
	}, nil
}

// MigrateTemplateArtifactToTC migrates one template's rootfs artifact to
// TC-backed storage and removes the local ext4 file on CubeMaster.
//
// The caller must pass a concrete template ID (resolve alias beforehand).
func MigrateTemplateArtifactToTC(ctx context.Context, templateID string) (*TemplateArtifactMigrationResult, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return nil, ErrTemplateIDRequired
	}
	result := &TemplateArtifactMigrationResult{TemplateID: templateID}
	if err := withTemplateWriteLock(templateID, func() error {
		def, err := GetDefinition(ctx, templateID)
		if err != nil {
			return err
		}
		if def.Status == StatusDeleting {
			return ErrTemplateNotFound
		}
		if def.Status != StatusReady {
			return ErrTemplateNotReady
		}
		artifactID := strings.TrimSpace(def.RootfsArtifactID)
		if artifactID == "" {
			return fmt.Errorf("template %s has no rootfs artifact id", templateID)
		}
		result.ArtifactID = artifactID
		artifact, err := getRootfsArtifactByID(ctx, artifactID)
		if err != nil {
			return err
		}

		// Already S3-backed: verify object existence, then clean local residue.
		if strings.TrimSpace(artifact.ArtifactURL) != "" {
			exists, statErr := statArtifactObjectInS3(ctx, artifactID)
			if statErr != nil {
				return fmt.Errorf("check s3 object for artifact %s: %w", artifactID, statErr)
			}
			if !exists {
				return fmt.Errorf("artifact %s is marked as s3-backed but object is missing; run `cubemastercli tpl migrate --template-id %s` to repair", artifactID, templateID)
			}
			cleaned, cleanErr := removeLocalArtifactFile(artifact.Ext4Path)
			if cleanErr != nil {
				// Best-effort: the artifact is already fully durable in S3, so
				// failing the whole job over a stale local file would be
				// misleading (retries would just repeat this same no-op
				// upload-skip). ext4_path is untouched in this branch, so the
				// next `tpl migrate` call retries this exact cleanup.
				log.G(ctx).Errorf("artifact %s already migrated to s3 but failed to remove stale local file %q; will retry on next `tpl migrate`: %v",
					artifactID, artifact.Ext4Path, cleanErr)
				result.Migrated = false
				result.Cleaned = false
				return nil
			}
			result.Migrated = false
			result.Cleaned = cleaned
			return nil
		}

		ext4Path := strings.TrimSpace(artifact.Ext4Path)
		if ext4Path == "" {
			return fmt.Errorf("artifact %s ext4_path is empty", artifactID)
		}
		info, err := os.Stat(ext4Path)
		if err != nil {
			if os.IsNotExist(err) {
				// Idempotency: in split-tier mode, a migrated artifact's ext4_path may
				// point to TC-local disk and therefore be absent on CubeMaster. If the
				// download path is still servable, treat this as already migrated.
				if artifactServedByRemoteTier() {
					if verifyErr := verifyArtifactServability(ctx, artifact); verifyErr == nil {
						result.Migrated = false
						result.Cleaned = false
						return nil
					}
				}
				return fmt.Errorf("artifact %s local ext4 is missing at %q", artifactID, ext4Path)
			}
			return fmt.Errorf("stat local ext4 %q for artifact %s: %w", ext4Path, artifactID, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact %s ext4 path %q is not a regular file", artifactID, ext4Path)
		}

		// Prefer S3 when configured; otherwise upload to TC's own local store.
		if err := uploadArtifactFileToS3(ctx, artifactID, ext4Path); err == nil {
			presignedURL, presignErr := presignArtifactGetURL(ctx, artifactID)
			if presignErr != nil {
				return fmt.Errorf("presign migrated artifact %s: %w", artifactID, presignErr)
			}
			// CAS on the status read at the top of this closure, not a blind
			// overwrite: withTemplateWriteLock only serializes callers within
			// THIS CubeMaster process. On another replica, a force-delete can
			// run concurrently and have TC's ArtifactDeleter claim (DELETING)
			// and remove this same row's data while this upload is still in
			// flight. A blind UPDATE would then resurrect the row as READY
			// pointing at data that was just deleted. Failing the update
			// instead leaves the (now orphaned) s3 object as the only cost.
			ok, updErr := updateRootfsArtifactIfStatus(ctx, artifactID, artifact.Status, map[string]any{
				"artifact_url": presignedURL,
				"status":       ArtifactStatusReady,
				"last_error":   "",
			})
			if updErr != nil {
				return fmt.Errorf("update rootfs artifact %s after s3 migration: %w", artifactID, updErr)
			}
			if !ok {
				return fmt.Errorf("artifact %s changed status concurrently (likely claimed for deletion on another replica); "+
					"the object was uploaded to s3 but the row was left untouched to avoid resurrecting a deleted artifact -- "+
					"the uploaded s3 object may be orphaned and require manual cleanup", artifactID)
			}
		} else {
			if !errors.Is(err, errS3PresignNotConfigured) {
				return err
			}
			uploadRes, uploadErr := uploadArtifactFileToTC(ctx, artifactID, ext4Path)
			if uploadErr != nil {
				return fmt.Errorf("upload artifact %s to template center store: %w", artifactID, uploadErr)
			}
			updates := map[string]any{
				"artifact_url": "",
				"ext4_path":    uploadRes.Ext4Path,
				"status":       ArtifactStatusReady,
				"last_error":   "",
			}
			if strings.TrimSpace(uploadRes.Ext4SHA256) != "" {
				updates["ext4_sha256"] = uploadRes.Ext4SHA256
			}
			if uploadRes.SizeBytes > 0 {
				updates["ext4_size_bytes"] = uploadRes.SizeBytes
			}
			// Best-effort cleanup of the OLD Master-local file BEFORE the row
			// stops pointing at it. Unlike the S3 branch, ext4_path is about
			// to be overwritten below, so a failure here can no longer be
			// retried via a future `tpl migrate` call (the old path is lost
			// from the DB) -- log loudly now since this is the only record of
			// the leak, but do not fail the job: the artifact is already
			// durably stored in TC, so failing here would only make an
			// operator retry an upload that has nothing left to do.
			if _, cleanErr := removeLocalArtifactFile(ext4Path); cleanErr != nil {
				log.G(ctx).Errorf("artifact %s migrated to template center store but failed to remove stale local file %q; manual cleanup required: %v",
					artifactID, ext4Path, cleanErr)
			}
			// Same cross-replica CAS reasoning as the s3 branch above.
			ok, updErr := updateRootfsArtifactIfStatus(ctx, artifactID, artifact.Status, updates)
			if updErr != nil {
				return fmt.Errorf("update rootfs artifact %s after tc-local migration: %w", artifactID, updErr)
			}
			if !ok {
				return fmt.Errorf("artifact %s changed status concurrently (likely claimed for deletion on another replica); "+
					"the object was uploaded to the template center store but the row was left untouched to avoid resurrecting "+
					"a deleted artifact -- the uploaded object may be orphaned and require manual cleanup", artifactID)
			}
			result.Migrated = true
			result.Cleaned = true
			return nil
		}

		// S3 branch only (the tc-local branch already returned above). Local
		// cleanup is best-effort here too: a failure must not fail the job --
		// the artifact is already durably migrated, and this branch does not
		// touch ext4_path, so the next `tpl migrate` call retries this exact
		// cleanup via the "already S3-backed" branch at the top.
		cleaned, cleanErr := removeLocalArtifactFile(ext4Path)
		if cleanErr != nil {
			log.G(ctx).Errorf("artifact %s migrated to s3 but failed to remove stale local file %q; will retry on next `tpl migrate`: %v",
				artifactID, ext4Path, cleanErr)
			result.Migrated = true
			result.Cleaned = false
			return nil
		}
		result.Migrated = true
		result.Cleaned = cleaned
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func removeLocalArtifactFile(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove local artifact file %q: %w", path, err)
	}
	_ = os.Remove(filepath.Dir(path))
	return true, nil
}

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

// validateTCLocalArtifactPath ensures the path TC reports back for a migrated
// local-disk artifact really is a file inside TC's own artifact store. The
// row update below persists this path verbatim; without this guard a
// misconfigured or malicious upload response could leave rootfs_artifacts
// pointing at an arbitrary host path, which later breaks download probes,
// reuse checks, and cleanup.
func validateTCLocalArtifactPath(artifactID, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" {
		return fmt.Errorf("template center returned an empty ext4 path for artifact %s", artifactID)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("template center returned non-absolute ext4 path %q for artifact %s", path, artifactID)
	}
	if filepath.Base(path) != artifactID+".ext4" || filepath.Base(filepath.Dir(path)) != artifactID {
		return fmt.Errorf("template center returned ext4 path %q that does not match the artifact layout for %s", path, artifactID)
	}
	roots := []string{ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, ArtifactFallbackStoreRootDir())
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("template center returned ext4 path %q outside the TC artifact store roots %v", path, roots)
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

	type artifactSnapshot struct {
		artifactID  string
		status      string
		artifactURL string
		ext4Path    string
	}
	snapshot := artifactSnapshot{}
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
		artifact, err := getRootfsArtifactByID(ctx, artifactID)
		if err != nil {
			return err
		}
		snapshot = artifactSnapshot{
			artifactID:  artifactID,
			status:      strings.TrimSpace(artifact.Status),
			artifactURL: strings.TrimSpace(artifact.ArtifactURL),
			ext4Path:    strings.TrimSpace(artifact.Ext4Path),
		}
		return nil
	}); err != nil {
		return nil, err
	}
	result.ArtifactID = snapshot.artifactID

	// Already S3-backed: verify object existence, then clean local residue.
	if snapshot.artifactURL != "" {
		exists, statErr := statArtifactObjectInS3(ctx, snapshot.artifactID)
		if statErr != nil {
			return nil, fmt.Errorf("check s3 object for artifact %s: %w", snapshot.artifactID, statErr)
		}
		if !exists {
			// Not "re-run the migration": the object is already gone, so there
			// is nothing left to upload and a re-run hits this same check.
			return nil, fmt.Errorf("artifact %s is marked as s3-backed but the object is missing from the bucket; "+
				"migration cannot repair it (there is nothing left to upload) -- rebuild the rootfs instead "+
				"(`cubemastercli tpl redo --template-id %s`)", snapshot.artifactID, templateID)
		}
		cleaned, cleanErr := removeLocalArtifactFile(snapshot.ext4Path)
		if cleanErr != nil {
			// Best-effort: the artifact is already fully durable in S3, so
			// failing the whole job over a stale local file would be misleading.
			log.G(ctx).Errorf("artifact %s already migrated to s3 but failed to remove stale local file %q; will retry on next `tpl merge`: %v",
				snapshot.artifactID, snapshot.ext4Path, cleanErr)
			result.Migrated = false
			result.Cleaned = false
			return result, nil
		}
		result.Migrated = false
		result.Cleaned = cleaned
		if result.Cleaned {
			invalidateTemplateCaches(templateID)
		}
		return result, nil
	}

	ext4Path := snapshot.ext4Path
	if ext4Path == "" {
		return nil, fmt.Errorf("artifact %s ext4_path is empty", snapshot.artifactID)
	}
	info, err := os.Stat(ext4Path)
	if err != nil {
		if os.IsNotExist(err) {
			// Idempotency: in split-tier mode, a migrated artifact's ext4_path may
			// point to TC-local disk and therefore be absent on CubeMaster.
			if artifactServedByRemoteTier() {
				artifact, loadErr := getRootfsArtifactByID(ctx, snapshot.artifactID)
				if loadErr == nil {
					if verifyErr := verifyArtifactServability(ctx, artifact); verifyErr == nil {
						result.Migrated = false
						result.Cleaned = false
						return result, nil
					}
				}
			}
			return nil, fmt.Errorf("artifact %s local ext4 is missing at %q", snapshot.artifactID, ext4Path)
		}
		return nil, fmt.Errorf("stat local ext4 %q for artifact %s: %w", ext4Path, snapshot.artifactID, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact %s ext4 path %q is not a regular file", snapshot.artifactID, ext4Path)
	}

	// Upload happens OUTSIDE the template write lock so long transfers do not
	// block unrelated writes (delete/redo/alias updates) for this template.
	updates := map[string]any{}
	uploadedToS3 := false
	targetExt4Path := ext4Path
	if err := uploadArtifactFileToS3(ctx, snapshot.artifactID, ext4Path); err == nil {
		presignedURL, presignErr := presignArtifactGetURL(ctx, snapshot.artifactID)
		if presignErr != nil {
			return nil, fmt.Errorf("presign migrated artifact %s: %w", snapshot.artifactID, presignErr)
		}
		uploadedToS3 = true
		updates = map[string]any{
			"artifact_url": presignedURL,
			"status":       ArtifactStatusReady,
			"last_error":   "",
		}
	} else {
		if !errors.Is(err, errS3PresignNotConfigured) {
			return nil, err
		}
		uploadRes, uploadErr := uploadArtifactFileToTC(ctx, snapshot.artifactID, ext4Path)
		if uploadErr != nil {
			return nil, fmt.Errorf("upload artifact %s to template center store: %w", snapshot.artifactID, uploadErr)
		}
		if err := validateTCLocalArtifactPath(snapshot.artifactID, uploadRes.Ext4Path); err != nil {
			return nil, fmt.Errorf("invalid template center upload path for artifact %s: %w", snapshot.artifactID, err)
		}
		targetExt4Path = uploadRes.Ext4Path
		updates = map[string]any{
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
	}

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
		if strings.TrimSpace(def.RootfsArtifactID) != snapshot.artifactID {
			return fmt.Errorf("template %s rootfs artifact changed from %s to %s while migration was in flight",
				templateID, snapshot.artifactID, strings.TrimSpace(def.RootfsArtifactID))
		}
		// Same cross-replica CAS reasoning as before: if a peer claimed this row
		// for deletion during the upload, do not resurrect it.
		ok, updErr := updateRootfsArtifactIfStatus(ctx, snapshot.artifactID, snapshot.status, updates)
		if updErr != nil {
			if uploadedToS3 {
				return fmt.Errorf("update rootfs artifact %s after s3 migration: %w", snapshot.artifactID, updErr)
			}
			return fmt.Errorf("update rootfs artifact %s after tc-local migration: %w", snapshot.artifactID, updErr)
		}
		if !ok {
			if uploadedToS3 {
				return fmt.Errorf("artifact %s changed status concurrently (likely claimed for deletion on another replica); "+
					"the object was uploaded to s3 but the row was left untouched to avoid resurrecting a deleted artifact -- "+
					"the uploaded s3 object may be orphaned and require manual cleanup", snapshot.artifactID)
			}
			return fmt.Errorf("artifact %s changed status concurrently (likely claimed for deletion on another replica); "+
				"the object was uploaded to the template center store but the row was left untouched to avoid resurrecting "+
				"a deleted artifact -- the uploaded object may be orphaned and require manual cleanup", snapshot.artifactID)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if !uploadedToS3 && shouldSkipLocalCleanup(ext4Path, targetExt4Path) {
		// Nothing was removed, but the row's stored path did change above.
		result.Migrated = true
		result.Cleaned = false
		invalidateTemplateCaches(templateID)
		return result, nil
	}

	// Local cleanup is best-effort in both branches.
	cleaned, cleanErr := removeLocalArtifactFile(ext4Path)
	if cleanErr != nil {
		if uploadedToS3 {
			log.G(ctx).Errorf("artifact %s migrated to s3 but failed to remove stale local file %q; will retry on next `tpl merge`: %v",
				snapshot.artifactID, ext4Path, cleanErr)
		} else {
			log.G(ctx).Errorf("artifact %s migrated to template center store but failed to remove stale local file %q; manual cleanup required: %v",
				snapshot.artifactID, ext4Path, cleanErr)
		}
		result.Migrated = true
		result.Cleaned = false
		invalidateTemplateCaches(templateID)
		return result, nil
	}
	result.Migrated = true
	result.Cleaned = cleaned
	invalidateTemplateCaches(templateID)
	return result, nil
}

func shouldSkipLocalCleanup(sourcePath, targetPath string) bool {
	sourcePath = strings.TrimSpace(sourcePath)
	targetPath = strings.TrimSpace(targetPath)
	if sourcePath == "" || targetPath == "" {
		return false
	}
	if filepath.Clean(sourcePath) == filepath.Clean(targetPath) {
		return true
	}
	// One physical file can still show up under two different path strings
	// (for example, a hard-linked staging file being ingested into the shared
	// artifact layout). String comparison cannot catch that; deleting the
	// source would remove the only copy the migrated row now points at.
	// Compare file identity before allowing the delete.
	sourceInfo, sourceErr := os.Stat(sourcePath)
	targetInfo, targetErr := os.Stat(targetPath)
	if sourceErr == nil && targetErr == nil && os.SameFile(sourceInfo, targetInfo) {
		return true
	}
	return false
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

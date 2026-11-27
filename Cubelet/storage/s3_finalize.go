// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// S3FinalizeResult is a compatibility alias for PackageFinalizeResult.
type S3FinalizeResult = PackageFinalizeResult

// FinalizeS3PackageSnapshots seals an S3 package: encode metadata onto its
// disk, seal memory/metadata work volumes, and clear the plain host metadata
// dir. Rootfs is already a snapshot. Non-S3 is a no-op.
//
// An unpersisted catalog leaves the package unsealed; every other failure is fatal.
func FinalizeS3PackageSnapshots(ctx context.Context, backend, snapshotID string) (S3FinalizeResult, error) {
	var res S3FinalizeResult
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil || normalized != cow.BackendS3 {
		return res, nil
	}
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return res, fmt.Errorf("snapshot_id is required")
	}
	store, err := requireCowStoreFor(cow.BackendS3)
	if err != nil {
		return res, err
	}
	s3store, ok := store.(*S3Cow)
	if !ok || s3store == nil {
		return res, fmt.Errorf("s3 cow store is not *S3Cow")
	}
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendS3, id)
	if err != nil || entry == nil {
		return res, fmt.Errorf("s3 package catalog %s: %w", id, err)
	}

	memoryWork := strings.TrimSpace(entry.MemoryVol)
	initialMetaRef := strings.TrimSpace(entry.MetadataVol)
	if initialMetaRef == "" {
		initialMetaRef = S3MetadataVolumeName(id)
	}

	metaDir := strings.TrimSpace(entry.MetaDir)
	if metaDir == "" {
		if home := strings.TrimSpace(entry.SnapshotPath); home != "" {
			metaDir = filepath.Join(home, SnapshotMetadataDir)
		} else {
			metaDir = SnapshotMetaDir(cow.BackendS3, SnapshotKindNormal, id)
		}
	}
	metaDir, err = validateS3MetadataDir(metaDir)
	if err != nil {
		return res, err
	}

	// Unmount any stale metadata disk left by an older Cubelet (retry/upgrade).
	s3MetadataMu.Lock()
	umountErr := unmountS3MetadataPathIfMounted(metaDir)
	s3MetadataMu.Unlock()
	if umountErr != nil {
		return res, umountErr
	}

	desiredMetaSnap := ""
	if initialMetaRef != "" && !IsS3MetadataBaseName(initialMetaRef) {
		desiredMetaSnap = S3MetadataSnapshotName(id)
		if strings.HasSuffix(initialMetaRef, "-snap") {
			desiredMetaSnap = initialMetaRef
		}
	}

	metaSnapAlreadyExists := false
	if desiredMetaSnap != "" {
		snapInfo, snapErr := s3store.GetVolumeInfo(ctx, desiredMetaSnap)
		snapExists, existsErr := cowObjectPresent(snapInfo, snapErr)
		if existsErr != nil {
			return res, existsErr
		}
		metaSnapAlreadyExists = snapExists
	}
	needPack := desiredMetaSnap != "" && !metaSnapAlreadyExists
	if needPack {
		if err := validateS3PackageDirForPack(metaDir, entry); err != nil {
			return res, fmt.Errorf("validate s3 metadata dir %s: %w", metaDir, err)
		}
	}

	// Point catalog at post-seal snap names before encoding.
	changed := false
	if memoryWork != "" && !strings.HasSuffix(memoryWork, "-snap") {
		entry.MemoryVol = memoryWork + "-snap"
		entry.MemoryKind = cowKindSnapshot
		changed = true
	} else if memoryWork != "" && strings.TrimSpace(entry.MemoryKind) != cowKindSnapshot {
		entry.MemoryKind = cowKindSnapshot
		changed = true
	}
	if desiredMetaSnap != "" {
		if strings.TrimSpace(entry.MetadataVol) != desiredMetaSnap || strings.TrimSpace(entry.MetadataKind) != cowKindSnapshot {
			entry.MetadataVol = desiredMetaSnap
			entry.MetadataKind = cowKindSnapshot
			changed = true
		}
	}
	if changed {
		if err := WriteSnapshotCatalogFor(cow.BackendS3, entry); err != nil {
			// Keep the package unsealed rather than packing a stale catalog.
			res.CleanupErr = fmt.Errorf("persist s3 package catalog %s: %w", id, err)
			return res, nil
		}
	}

	// Encode MetaDir into the work volume without mounting it (skip if sealed).
	packWorkVol := ""
	if needPack {
		if err := validateOptionalCatalogFile(metaDir); err != nil {
			return res, err
		}
		packWorkVol = initialMetaRef
		if strings.HasSuffix(packWorkVol, "-snap") {
			packWorkVol = S3MetadataVolumeName(id)
		}
		if err := s3store.packS3MetadataVolume(ctx, packWorkVol, metaDir); err != nil {
			return res, fmt.Errorf("pack s3 metadata %s: %w", packWorkVol, err)
		}
	}

	if memoryWork != "" {
		desiredMemSnap := memoryWork
		if !strings.HasSuffix(desiredMemSnap, "-snap") {
			desiredMemSnap = memoryWork + "-snap"
		}
		if strings.HasSuffix(memoryWork, "-snap") {
			snapInfo, snapErr := s3store.GetVolumeInfo(ctx, desiredMemSnap)
			snapExists, existsErr := cowObjectPresent(snapInfo, snapErr)
			if existsErr != nil {
				if packWorkVol != "" {
					_ = deactivateS3Object(ctx, s3store, packWorkVol, cowKindVolume)
				}
				return res, existsErr
			}
			if !snapExists {
				memoryWork = strings.TrimSuffix(memoryWork, "-snap")
			}
		}
		snap, _, sealErr := s3store.sealVolumeToSnapshotAs(ctx, memoryWork, desiredMemSnap)
		if sealErr != nil {
			if packWorkVol != "" {
				_ = deactivateS3Object(ctx, s3store, packWorkVol, cowKindVolume)
			}
			return res, fmt.Errorf("seal memory %s: %w", memoryWork, sealErr)
		}
		if snap != "" {
			entry.MemoryVol = snap
			entry.MemoryKind = cowKindSnapshot
		}
	}
	if desiredMetaSnap != "" {
		if metaSnapAlreadyExists {
			entry.MetadataVol = desiredMetaSnap
			entry.MetadataKind = cowKindSnapshot
		} else {
			snap, _, sealErr := s3store.sealVolumeToSnapshotAs(ctx, packWorkVol, desiredMetaSnap)
			if sealErr != nil {
				_ = deactivateS3Object(ctx, s3store, packWorkVol, cowKindVolume)
				return res, fmt.Errorf("seal metadata %s: %w", packWorkVol, sealErr)
			}
			if snap != "" {
				entry.MetadataVol = snap
				entry.MetadataKind = cowKindSnapshot
			}
		}
	}

	res.Sealed = true

	// Post-seal cleanup failures are non-fatal (res.CleanupErr).
	var cleanupErr error
	memorySnap := strings.TrimSpace(entry.MemoryVol)
	if memoryWork != "" && memoryWork != memorySnap && !strings.HasSuffix(memoryWork, "-snap") {
		if err := deactivateS3Object(ctx, s3store, memoryWork, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deactivate memory work %s: %w", memoryWork, err))
		}
		if err := s3store.DeleteByKind(ctx, memoryWork, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete memory work %s: %w", memoryWork, err))
		}
	}
	if memorySnap != "" && strings.HasSuffix(memorySnap, "-snap") {
		if err := deactivateS3Object(ctx, s3store, memorySnap, cowKindSnapshot); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deactivate memory snap %s: %w", memorySnap, err))
		}
	}
	metaSnap := strings.TrimSpace(entry.MetadataVol)
	// Reclaim the work volume even when packing is skipped.
	metaWorkVol := S3MetadataVolumeName(id)
	if metaWorkVol != metaSnap {
		if err := deactivateS3Object(ctx, s3store, metaWorkVol, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deactivate metadata work %s: %w", metaWorkVol, err))
		}
		if err := s3store.DeleteByKind(ctx, metaWorkVol, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete metadata work %s: %w", metaWorkVol, err))
		}
	}
	if metaSnap != "" && !IsS3MetadataBaseName(metaSnap) {
		if err := deactivateS3Object(ctx, s3store, metaSnap, cowKindSnapshot); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deactivate metadata snap %s: %w", metaSnap, err))
		}
	}
	if err := dropDerived(id); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("drop s3 metadata derived %s: %w", id, err))
	}

	// Only clear the plain host directory once the sealed disk holds a copy.
	metaSealed := packWorkVol != "" || metaSnapAlreadyExists
	if metaSealed {
		if err := resetDir(metaDir); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clear s3 metadata dir %s: %w", metaDir, err))
		}
	}
	res.CleanupErr = cleanupErr
	return res, nil
}

// validateS3PackageDirForPack checks metaDir is symlink-free and has the
// required metadata files for the package kind and memory mode.
func validateS3PackageDirForPack(metaDir string, entry *SnapshotCatalogEntry) error {
	if err := filepath.WalkDir(metaDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("s3 metadata entry %s must not be a symlink", path)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("s3 metadata entry %s is not a regular file", path)
		}
		return nil
	}); err != nil {
		return err
	}
	required := []string{"metadata.json", filepath.Join("snapshot", "config.json")}
	if entry != nil && strings.TrimSpace(entry.MemoryVol) != "" {
		required = append(required, filepath.Join("snapshot", "state.json"))
	}
	if entry != nil && entry.Kind == CatalogKindPauseSnapshot {
		required = append(required, "sandbox_spec.json")
	}
	for _, rel := range required {
		if err := validateJSONObjectFile(filepath.Join(metaDir, rel)); err != nil {
			return fmt.Errorf("validate s3 package %s: %w", rel, err)
		}
	}
	return nil
}

// validateOptionalCatalogFile allows a missing catalog.json but not a corrupt one.
func validateOptionalCatalogFile(metaDir string) error {
	err := validateJSONObjectFile(filepath.Join(metaDir, snapshotCatalogFileName))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("validate s3 package %s: %w", snapshotCatalogFileName, err)
}

// resetDir clears dir's contents while preserving dir itself.
func resetDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	clean, err := validateS3MetadataDir(dir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(clean, entry.Name())); err != nil && firstErr == nil { // NOCC:Path Traversal()
			firstErr = err
		}
	}
	return firstErr
}

func (m *S3Cow) sealVolumeToSnapshotAs(ctx context.Context, volumeName, snapName string) (string, bool, error) {
	_ = ctx
	volumeName = strings.TrimSpace(volumeName)
	snapName = strings.TrimSpace(snapName)
	if volumeName == "" || snapName == "" {
		return "", false, fmt.Errorf("volume and snapshot names are required")
	}
	if IsS3MetadataBaseName(volumeName) || IsS3MetadataBaseName(snapName) {
		return "", false, fmt.Errorf("refusing to seal node-local s3 metadata base")
	}
	info, infoErr := m.GetVolumeInfo(ctx, snapName)
	exists, err := cowObjectPresent(info, infoErr)
	if err != nil {
		return "", false, err
	}
	if exists {
		return snapName, false, nil
	}
	// If the catalog already points at a snapshot under volumeName, keep it.
	if volumeName == snapName {
		return snapName, false, nil
	}
	srcInfo, srcErr := m.GetVolumeInfo(ctx, volumeName)
	srcExists, srcPresentErr := cowObjectPresent(srcInfo, srcErr)
	if srcPresentErr != nil {
		return "", false, srcPresentErr
	}
	if !srcExists {
		return "", false, fmt.Errorf("%w: %s", ErrCowObjectMissing, volumeName)
	}
	if _, err := m.engine.CreateSnapshotFromVolume(volumeName, snapName, false); err != nil {
		if !isCowSemantic(err, cubecow.SemAlreadyExists) {
			// Source may already be a snapshot — treat as sealed under volumeName.
			if isCowSemantic(err, cubecow.SemInvalidArgument) {
				return volumeName, false, nil
			}
			return "", false, err
		}
	}
	return snapName, true, nil
}

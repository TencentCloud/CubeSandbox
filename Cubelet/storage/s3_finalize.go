// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// FinalizeS3PackageSnapshots is a Pause／Commit／AppSnapshot packaging step:
// flush＋umount the package metadata mount, seal RW memory／metadata work
// volumes into RO snapshots, deactivate the sealed snaps (no host IO until a
// later restore ResolveDevPath／clone), and delete the work volumes
// (snapshots remain; volumes follow the sandbox／build lifecycle).
//
// Metadata must be umounted before seal: CreateSnapshotFromVolume captures the
// block device as-is, and a live mount can leave metadata.json／catalog.json in
// the page cache so the sealed snap is missing them (shim then fails restore).
// Catalog is rewritten to snap names while still mounted, then umount flushes.
// Rootfs is already a snapshot from CommitTemplateRootfs. XFS is a no-op.
func FinalizeS3PackageSnapshots(ctx context.Context, backend, snapshotID string) error {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil || normalized != cow.BackendS3 {
		return nil
	}
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return fmt.Errorf("snapshot_id is required")
	}
	store, err := requireCowStoreFor(cow.BackendS3)
	if err != nil {
		return err
	}
	s3store, ok := store.(*S3Cow)
	if !ok || s3store == nil {
		return fmt.Errorf("s3 cow store is not *S3Cow")
	}
	entry, err := GetLocalSnapshotFor(ctx, cow.BackendS3, id)
	if err != nil || entry == nil {
		return fmt.Errorf("s3 package catalog %s: %w", id, err)
	}

	memoryWork := strings.TrimSpace(entry.MemoryVol)
	metaWork := strings.TrimSpace(entry.MetadataVol)
	if metaWork == "" {
		metaWork = S3MetadataVolumeName(id)
	}

	// Point catalog at the post-seal snap names while MetaDir is still mounted
	// so metadata.json／catalog.json land on the volume before umount＋seal.
	changed := false
	if memoryWork != "" && !strings.HasSuffix(memoryWork, "-snap") {
		entry.MemoryVol = memoryWork + "-snap"
		entry.MemoryKind = cowKindSnapshot
		changed = true
	} else if memoryWork != "" && strings.TrimSpace(entry.MemoryKind) != cowKindSnapshot {
		entry.MemoryKind = cowKindSnapshot
		changed = true
	}
	desiredMetaSnap := ""
	if metaWork != "" && !IsS3MetadataBaseName(metaWork) {
		desiredMetaSnap = S3MetadataSnapshotName(id)
		if strings.HasSuffix(metaWork, "-snap") {
			desiredMetaSnap = metaWork
		}
		if strings.TrimSpace(entry.MetadataVol) != desiredMetaSnap || strings.TrimSpace(entry.MetadataKind) != cowKindSnapshot {
			entry.MetadataVol = desiredMetaSnap
			entry.MetadataKind = cowKindSnapshot
			changed = true
		}
	}
	if changed {
		if err := WriteSnapshotCatalogFor(cow.BackendS3, entry); err != nil {
			return err
		}
	}

	metaDir := strings.TrimSpace(entry.MetaDir)
	if metaDir == "" {
		if home := strings.TrimSpace(entry.SnapshotPath); home != "" {
			metaDir = filepath.Join(home, SnapshotMetadataDir)
		} else {
			metaDir = SnapshotMetaDir(cow.BackendS3, SnapshotKindNormal, id)
		}
	}
	if err := UnmountS3Metadata(metaDir); err != nil {
		return fmt.Errorf("umount s3 metadata before seal %s: %w", metaDir, err)
	}
	if pauseHome := SnapshotHome(cow.BackendS3, SnapshotKindPause, id); pauseHome != "" {
		_ = UnmountS3Metadata(filepath.Join(pauseHome, SnapshotMetadataDir))
	}

	if memoryWork != "" {
		snap, _, sealErr := s3store.sealVolumeToSnapshot(ctx, memoryWork)
		if sealErr != nil {
			return fmt.Errorf("seal memory %s: %w", memoryWork, sealErr)
		}
		if snap != "" {
			entry.MemoryVol = snap
			entry.MemoryKind = cowKindSnapshot
		}
	}
	if metaWork != "" && !IsS3MetadataBaseName(metaWork) && desiredMetaSnap != "" {
		info, infoErr := s3store.GetVolumeInfo(ctx, metaWork)
		exists, existsErr := cowObjectPresent(info, infoErr)
		if existsErr != nil {
			return existsErr
		}
		if exists {
			snap, _, sealErr := s3store.sealVolumeToSnapshotAs(ctx, metaWork, desiredMetaSnap)
			if sealErr != nil {
				return fmt.Errorf("seal metadata %s: %w", metaWork, sealErr)
			}
			if snap != "" {
				entry.MetadataVol = snap
				entry.MetadataKind = cowKindSnapshot
			}
		} else if snapInfo, snapErr := s3store.GetVolumeInfo(ctx, desiredMetaSnap); snapErr == nil && snapInfo != nil {
			entry.MetadataVol = desiredMetaSnap
			entry.MetadataKind = cowKindSnapshot
		}
	}

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
	if metaWork != "" && metaWork != metaSnap && !IsS3MetadataBaseName(metaWork) && !strings.HasSuffix(metaWork, "-snap") {
		if err := deactivateS3Object(ctx, s3store, metaWork, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("deactivate metadata work %s: %w", metaWork, err))
		}
		if err := s3store.DeleteByKind(ctx, metaWork, cowKindVolume); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete metadata work %s: %w", metaWork, err))
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
	return cleanupErr
}

// sealVolumeToSnapshot creates name+"-snap" from a RW volume. If name is
// already a snapshot, returns name unchanged.
func (m *S3Cow) sealVolumeToSnapshot(ctx context.Context, volumeName string) (snapName string, sealed bool, err error) {
	return m.sealVolumeToSnapshotAs(ctx, volumeName, strings.TrimSpace(volumeName)+"-snap")
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

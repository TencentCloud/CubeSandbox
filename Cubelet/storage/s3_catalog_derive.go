// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

type s3ObjectCandidate struct {
	name string
	kind string
}

// deriveS3CatalogEntry rebuilds a package catalog entry from its id alone.
//
// catalog.json lives on the package metadata disk, which Finalize seals and
// unmounts, so after a Cubelet restart no host path holds it any more. Every
// cubecow object name in a package is a pure function of the package id, so
// probe the S3 store for the objects that exist instead of activating and
// mounting the sealed metadata snap back just to read one file.
//
// The rootfs object is the package's proof of existence: without it the id
// names no package on this node and the catalog miss stays a miss.
func deriveS3CatalogEntry(ctx context.Context, snapshotID string) (*SnapshotCatalogEntry, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, ErrSnapshotCatalogNotFound
	}
	store, err := requireS3Cow()
	if err != nil {
		return nil, ErrSnapshotCatalogNotFound
	}
	rootfs := cowTemplateRootfsName(snapshotID)
	info, infoErr := store.GetVolumeInfo(ctx, rootfs)
	exists, err := cowObjectPresent(info, infoErr)
	if err != nil || !exists {
		return nil, ErrSnapshotCatalogNotFound
	}

	home, kind := s3PackageHomeAndKind(snapshotID)
	entry := &SnapshotCatalogEntry{
		SnapshotID:   snapshotID,
		SnapshotPath: home,
		MetaDir:      filepath.Join(home, SnapshotMetadataDir),
		RootfsVol:    rootfs,
		RootfsKind:   cowKindSnapshot,
		Kind:         kind,
		Backend:      cow.BackendS3,
	}
	if info != nil {
		entry.RootfsSizeBytes = info.SizeBytes
	}
	memWork := cowTemplateMemoryName(snapshotID)
	if name, memKind := firstExistingS3Object(ctx, store,
		s3ObjectCandidate{name: memWork + "-snap", kind: cowKindSnapshot},
		s3ObjectCandidate{name: memWork, kind: cowKindVolume},
	); name != "" {
		entry.MemoryVol = name
		entry.MemoryKind = memKind
	}
	if name, metaKind := firstExistingS3Object(ctx, store,
		s3ObjectCandidate{name: S3MetadataSnapshotName(snapshotID), kind: cowKindSnapshot},
		s3ObjectCandidate{name: S3MetadataVolumeName(snapshotID), kind: cowKindVolume},
	); name != "" && !IsS3MetadataBaseName(name) {
		entry.MetadataVol = name
		entry.MetadataKind = metaKind
	}
	return entry, nil
}

// firstExistingS3Object returns the first candidate present in cubecow. A
// lookup error is treated as absent: the caller is already on the degraded
// path and a partial entry beats failing the whole restore.
func firstExistingS3Object(ctx context.Context, store *S3Cow, candidates ...s3ObjectCandidate) (string, string) {
	for _, c := range candidates {
		if strings.TrimSpace(c.name) == "" {
			continue
		}
		info, infoErr := store.GetVolumeInfo(ctx, c.name)
		exists, err := cowObjectPresent(info, infoErr)
		if err != nil || !exists {
			continue
		}
		return c.name, c.kind
	}
	return "", ""
}

// s3PackageHomeAndKind locates the package directory on this node. The
// parent directory name (snapshots vs pause-snapshots) is what tells a pause
// package apart from a template／runtime snapshot; ids alone do not.
func s3PackageHomeAndKind(snapshotID string) (string, string) {
	roots := snapshotCatalogRootsSnapshot(cow.BackendS3)
	for _, root := range roots {
		home := filepath.Join(root, snapshotID)
		if st, err := os.Stat(home); err != nil || !st.IsDir() {
			continue
		}
		return home, s3PackageKindForRoot(root, snapshotID)
	}
	home := S3SnapshotHome(snapshotID)
	return home, s3PackageKindForRoot(filepath.Dir(home), snapshotID)
}

func s3PackageKindForRoot(root, snapshotID string) string {
	if filepath.Base(filepath.Clean(root)) == SnapshotKindPause {
		return CatalogKindPauseSnapshot
	}
	if strings.HasPrefix(snapshotID, "tpl-") {
		return CatalogKindTemplate
	}
	return CatalogKindRuntimeSnapshot
}

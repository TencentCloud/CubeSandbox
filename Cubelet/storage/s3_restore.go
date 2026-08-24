// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// Activate implements [cow.Activator]. Opens cubecow objects that
// already exist locally. Missing objects fail — there is no remote ingest.
// Does not start a sandbox. Callers decide which package objects to open
// (via catalog／refs); this method does not apply role-specific policy.
func (m *S3Cow) Activate(ctx context.Context, snapshotID string) error {
	return activateStoreObjects(ctx, m, cow.BackendS3, snapshotID)
}

var _ cow.Activator = (*S3Cow)(nil)

func activateStoreObjects(ctx context.Context, store cow.Store, backend, snapshotID string) error {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return fmt.Errorf("snapshot_id is required")
	}
	refs := activateObjectRefs(ctx, backend, id)
	if len(refs) == 0 {
		return fmt.Errorf("%w: snapshot %s has no rootfs/memory objects", ErrCowObjectMissing, id)
	}
	for _, ref := range refs {
		if IsS3MetadataBaseName(ref.Name) {
			continue
		}
		info, err := store.GetVolumeInfo(ctx, ref.Name)
		if err != nil {
			if ref.Role == "metadata" && isCowSemantic(err, cubecow.SemNotFound) {
				continue
			}
			return err
		}
		if info == nil {
			if ref.Role == "metadata" {
				continue
			}
			return fmt.Errorf("%w: snapshot %s object %s", ErrCowObjectMissing, id, ref.Name)
		}
		// Metadata IO always goes through a cloned RW volume (s3-meta-<id>).
		// Activating the sealed package snap here leaks an NVMe device with
		// no mount／reader.
		if ref.Role == "metadata" {
			continue
		}
		if _, err := store.ResolveDevPath(ctx, ref.Name, ref.Kind); err != nil {
			return fmt.Errorf("activate snapshot %s object %s: %w", id, ref.Name, err)
		}
	}
	return nil
}

func activateObjectRefs(ctx context.Context, backend, snapshotID string) []CowObjectRef {
	entry, err := GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err == nil && entry != nil {
		refs := make([]CowObjectRef, 0, 3)
		if name := strings.TrimSpace(entry.RootfsVol); name != "" {
			kind := strings.TrimSpace(entry.RootfsKind)
			if kind == "" {
				kind = cowKindSnapshot
			}
			refs = append(refs, CowObjectRef{Name: name, Kind: kind, Role: "rootfs"})
		}
		if name := strings.TrimSpace(entry.MemoryVol); name != "" {
			kind := strings.TrimSpace(entry.MemoryKind)
			if kind == "" {
				kind = cowKindVolume
			}
			refs = append(refs, CowObjectRef{Name: name, Kind: kind, Role: "memory"})
		}
		if name := strings.TrimSpace(entry.MetadataVol); name != "" && !IsS3MetadataBaseName(name) {
			kind := strings.TrimSpace(entry.MetadataKind)
			if kind == "" {
				kind = cowKindSnapshot
			}
			refs = append(refs, CowObjectRef{Name: name, Kind: kind, Role: "metadata"})
		} else if isS3CatalogBackend(backend) {
			if meta := S3MetadataSnapshotName(entry.SnapshotID); meta != "" && !IsS3MetadataBaseName(meta) {
				refs = append(refs, CowObjectRef{Name: meta, Kind: cowKindSnapshot, Role: "metadata"})
			}
		}
		if len(refs) > 0 {
			return refs
		}
	}
	refs := []CowObjectRef{
		{Name: cowTemplateRootfsName(snapshotID), Kind: cowKindSnapshot, Role: "rootfs"},
		{Name: cowTemplateMemoryName(snapshotID), Kind: cowKindVolume, Role: "memory"},
	}
	if isS3CatalogBackend(backend) {
		if meta := S3MetadataSnapshotName(snapshotID); meta != "" && !IsS3MetadataBaseName(meta) {
			refs = append(refs, CowObjectRef{Name: meta, Kind: cowKindSnapshot, Role: "metadata"})
		}
	}
	return refs
}

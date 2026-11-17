// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
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
	refs, err := activateObjectRefs(ctx, store, backend, id)
	if err != nil {
		return fmt.Errorf("resolve snapshot %s objects: %w", id, err)
	}
	// The rootfs is the package's proof of existence: memory or metadata
	// alone name a partial package, not something that can be activated.
	if !hasRootfsRef(refs) {
		return fmt.Errorf("%w: snapshot %s has no rootfs object", ErrCowObjectMissing, id)
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

// activateObjectRefs returns the objects of a package in the order callers
// activate or export them (rootfs, memory, metadata).
//
// The catalog is the authority on what a package contains: a filesystem-only
// package has no memory object, and inventing one would turn a valid package
// into a dangling reference. When the catalog cannot be read, the object names
// are still a pure function of the package id, so probe the store and keep only
// names that exist — the contract ADR-14 states for the degraded path.
func activateObjectRefs(ctx context.Context, store cow.Store, backend, snapshotID string) ([]CowObjectRef, error) {
	entry, err := GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err != nil && !errors.Is(err, ErrSnapshotCatalogNotFound) {
		// A broken catalog must not fall back to name guessing: the
		// conventional names could belong to a different package.
		return nil, fmt.Errorf("load snapshot %s catalog: %w", snapshotID, err)
	}
	if err == nil && entry != nil {
		if refs := catalogObjectRefs(entry, backend); len(refs) > 0 {
			return refs, nil
		}
	}
	return fallbackObjectRefs(ctx, store, backend, snapshotID)
}

func hasRootfsRef(refs []CowObjectRef) bool {
	for _, ref := range refs {
		if ref.Role == "rootfs" && strings.TrimSpace(ref.Name) != "" {
			return true
		}
	}
	return false
}

// catalogObjectRefs lists the objects a readable catalog entry names.
func catalogObjectRefs(entry *SnapshotCatalogEntry, backend string) []CowObjectRef {
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
	return refs
}

// fallbackObjectRefs derives a package's objects from its id alone, using the
// same name table as deriveS3CatalogEntry so the two degraded paths cannot
// disagree about what a sealed package looks like.
func fallbackObjectRefs(ctx context.Context, store cow.Store, backend, snapshotID string) ([]CowObjectRef, error) {
	rootfs, _, err := firstExistingCowObject(ctx, store,
		s3ObjectCandidate{name: cowTemplateRootfsName(snapshotID), kind: cowKindSnapshot},
	)
	if err != nil {
		return nil, err
	}
	if rootfs == "" {
		// No rootfs means this id names no package on this node, even if a
		// leftover memory or metadata object still carries the name.
		return nil, nil
	}
	refs := []CowObjectRef{{Name: rootfs, Kind: cowKindSnapshot, Role: "rootfs"}}
	memory, memoryKind, err := firstExistingCowObject(ctx, store, packageMemoryCandidates(snapshotID)...)
	if err != nil {
		return nil, err
	}
	if memory != "" {
		refs = append(refs, CowObjectRef{Name: memory, Kind: memoryKind, Role: "memory"})
	}
	if isS3CatalogBackend(backend) {
		// Probe, do not assume: a phantom metadata ref would make an
		// otherwise empty ref list look like a package.
		meta, metaKind, err := firstExistingCowObject(ctx, store, packageMetadataCandidates(snapshotID)...)
		if err != nil {
			return nil, err
		}
		if meta != "" && !IsS3MetadataBaseName(meta) {
			refs = append(refs, CowObjectRef{Name: meta, Kind: metaKind, Role: "metadata"})
		}
	}
	return refs, nil
}

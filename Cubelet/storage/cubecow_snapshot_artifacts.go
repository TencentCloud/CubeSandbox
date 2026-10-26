// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

const (
	CowKindVolume   = cow.KindVolume
	CowKindSnapshot = cow.KindSnapshot
)

// cowMetricKeys mirrors the keys the cubecow Rust crate emits via
// `cubecow_get_metrics()` for the reflink-only backend. The legacy
// dm-thin pool_* keys are no longer surfaced.
var cowMetricKeys = []string{
	"total_bytes",
	"used_bytes",
	"volume_count",
	"snapshot_count",
}

type CowSnapshotObject struct {
	Name      string
	MountName string
	Kind      string
	DevPath   string
	SizeBytes uint64
	Gen       uint32
}

type CowRollbackSnapshotRefs struct {
	Rootfs *CowSnapshotObject
	Memory *CowSnapshotObject
}

type CowObjectRef struct {
	Name string
	Kind string
	Role string
}

type CowObjectStatus struct {
	Name         string
	Kind         string
	Role         string
	Exists       bool
	DevicePath   string
	SizeBytes    uint64
	ErrorMessage string
}

func IsCowBackend() bool {
	return localStorage != nil && localStorage.useCowStorage()
}

// ActiveCowStore returns the default (XFS) CoW Store, or nil when storage is
// not initialized / not using a CoW backend. Prefer [StoreFor] when the request
// carries an explicit backend type (xfs｜s3).
func ActiveCowStore() cow.Store {
	if localStorage == nil {
		return nil
	}
	return localStorage.cowManager
}

// ActiveS3CowStore returns the S3 CoW Store, or nil when unset.
func ActiveS3CowStore() cow.Store {
	if localStorage == nil {
		return nil
	}
	return localStorage.s3CowManager
}

// Compatibility aliases — prefer GetSandboxRootfs / CommitRootfs / CreateMemoryVolume /
// CommitMemoryFromBase / CommitRootfsFromBuild / DeleteObject / DeactivateObject /
// ResolveObjectPath / CleanupObjects / InspectObjects / ObjectMetrics.

func GetSandboxRootfsForSnapshot(ctx context.Context, sandboxID, preferredVolumeName string) (*CowSnapshotObject, error) {
	return GetSandboxRootfs(ctx, sandboxID, preferredVolumeName)
}

func CommitTemplateRootfs(ctx context.Context, source *CowSnapshotObject, templateID string) (*CowSnapshotObject, error) {
	return CommitRootfs(ctx, source, templateID)
}

func CreateTemplateRootfsFromBuild(ctx context.Context, templateID string) (*CowSnapshotObject, error) {
	return CommitRootfsFromBuild(ctx, templateID)
}

func CreateTemplateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*CowSnapshotObject, error) {
	return CreateMemoryVolume(ctx, templateID, sizeBytes)
}

func CommitTemplateMemoryFromBase(ctx context.Context, source *CowSnapshotObject, templateID string, sizeBytes uint64) (*CowSnapshotObject, error) {
	return CommitMemoryFromBase(ctx, source, templateID, sizeBytes)
}

func DefaultTemplateObjectRefs(templateID string) []CowObjectRef {
	return []CowObjectRef{
		{Name: cowTemplateRootfsName(templateID), Kind: cowKindSnapshot, Role: "rootfs"},
		{Name: cowTemplateMemoryName(templateID), Kind: cowKindVolume, Role: "memory"},
		{Name: cowTemplateMemoryName(templateID) + "-snap", Kind: cowKindSnapshot, Role: "memory"},
		{Name: cowTemplateBuildRootfsName(templateID), Kind: cowKindVolume, Role: "build_rootfs"},
		{Name: S3MetadataVolumeName(templateID), Kind: cowKindVolume, Role: "metadata"},
		{Name: S3MetadataSnapshotName(templateID), Kind: cowKindSnapshot, Role: "metadata"},
	}
}

// AppendS3SealedPackageCleanupRefs adds the sealed package names that
// Finalize leaves behind (memory-snap, metadata work／snap). Catalog entries
// often list only the live MemoryVol and miss the -snap after umount.
func AppendS3SealedPackageCleanupRefs(templateID string, refs []CowObjectRef) []CowObjectRef {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return refs
	}
	seen := make(map[string]struct{}, len(refs)+4)
	out := make([]CowObjectRef, 0, len(refs)+4)
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, ref)
	}
	for _, extra := range []CowObjectRef{
		{Name: cowTemplateMemoryName(templateID) + "-snap", Kind: cowKindSnapshot, Role: "memory"},
		{Name: cowTemplateMemoryName(templateID), Kind: cowKindVolume, Role: "memory"},
		{Name: S3MetadataVolumeName(templateID), Kind: cowKindVolume, Role: "metadata"},
		{Name: S3MetadataSnapshotName(templateID), Kind: cowKindSnapshot, Role: "metadata"},
	} {
		if _, ok := seen[extra.Name]; ok {
			continue
		}
		seen[extra.Name] = struct{}{}
		out = append(out, extra)
	}
	return out
}

// TemplateBuildRootfsName returns the deterministic cubecow volume name used
// for a template's temporary writable working layer during AppSnapshot. Exposed
// so non-storage callers (e.g. AppSnapshot handler writing snapshot catalog)
// can record the name without redeclaring the format string.
func TemplateBuildRootfsName(templateID string) string {
	return cowTemplateBuildRootfsName(templateID)
}

// ResolveSnapshotForRollback resolves the cubecow objects that back a
// snapshot for the purposes of rollback. memoryKind is honored so that
// snapshot replicas whose memory blob was produced by reflink-clone
// (kind=snapshot, used for incremental memory snapshots) work alongside
// the legacy empty-volume layout (kind=volume).
func ResolveSnapshotForRollback(ctx context.Context, rootfsVol, memoryVol, memoryKind string) (*CowRollbackSnapshotRefs, error) {
	return ResolveRollbackRefs(ctx, rootfsVol, memoryVol, memoryKind)
}

// resolveRollbackMemoryKind defaults to the legacy "volume" kind so that
// callers (and historical catalog entries) that omit the kind continue to
// behave like before. Catalog entries committed under the new incremental
// flow record kind=snapshot, which is preserved here verbatim.
func resolveRollbackMemoryKind(kind string) (string, error) {
	trimmed := strings.TrimSpace(kind)
	if trimmed == "" {
		return cowKindVolume, nil
	}
	return normalizeCowKind(trimmed)
}

func RollbackDeriveNewGen(ctx context.Context, sandboxID, snapshotRootfsVol string, newGen uint32, desiredSizeBytes uint64) (*CowSnapshotObject, error) {
	return DeriveRollbackRootfs(ctx, sandboxID, snapshotRootfsVol, newGen, desiredSizeBytes)
}

func PersistSandboxRootfsAfterRollback(ctx context.Context, sandboxID string, rootfs *CowSnapshotObject) error {
	return PersistSandboxRootfs(ctx, sandboxID, rootfs)
}

func DeleteCowObject(ctx context.Context, name, kind string) error {
	return DeleteObject(ctx, name, kind)
}

func DeactivateCowObject(ctx context.Context, name, kind string) error {
	return DeactivateObject(ctx, name, kind)
}

func ResolveCowDevPath(ctx context.Context, name, kind string) (string, error) {
	return ResolveObjectPath(ctx, name, kind)
}

func CleanupCowTemplateObjects(ctx context.Context, refs []CowObjectRef) error {
	return CleanupObjects(ctx, refs)
}

func InspectCowObjects(ctx context.Context, refs []CowObjectRef) ([]CowObjectStatus, error) {
	return InspectObjects(ctx, refs)
}

func GetCowMetrics(ctx context.Context) (map[string]uint64, error) {
	return ObjectMetrics(ctx)
}

func selectSnapshotRootfs(info *StorageInfo, preferredVolumeName string) (*BackendFileInfo, error) {
	if info == nil || len(info.Volumes) == 0 {
		return nil, fmt.Errorf("sandbox storage info has no volumes")
	}
	preferredVolumeName = strings.TrimSpace(preferredVolumeName)
	if preferredVolumeName != "" {
		volume := info.Volumes[preferredVolumeName]
		if volume == nil || volume.VolumeName == "" {
			return nil, fmt.Errorf("rootfs volume %q is not backed by cubecow", preferredVolumeName)
		}
		return volume, nil
	}

	var rootfs *BackendFileInfo
	for _, volume := range info.Volumes {
		if volume == nil || volume.VolumeName == "" {
			continue
		}
		if strings.HasPrefix(volume.VolumeName, fmt.Sprintf("sb-%s-rootfs-gen", info.SandboxID)) {
			if rootfs != nil {
				return nil, fmt.Errorf("multiple cubecow rootfs candidates for sandbox %s", info.SandboxID)
			}
			rootfs = volume
		}
	}
	if rootfs == nil {
		return nil, fmt.Errorf("sandbox %s has no cubecow rootfs candidate", info.SandboxID)
	}
	return rootfs, nil
}

func backendFileInfoToSnapshotObject(ctx context.Context, manager cowVolumeManager, info *BackendFileInfo) (*CowSnapshotObject, error) {
	if info == nil {
		return nil, fmt.Errorf("backend file info is nil")
	}
	obj := &CowSnapshotObject{
		Name:      info.VolumeName,
		MountName: info.Name,
		Kind:      info.Kind,
		DevPath:   info.FilePath,
		Gen:       info.Gen,
	}
	if obj.DevPath == "" && obj.Name != "" {
		devPath, err := manager.ResolveDevPath(ctx, obj.Name, obj.Kind)
		if err != nil {
			return nil, err
		}
		obj.DevPath = devPath
	}
	if obj.Name != "" {
		size, err := manager.GetSizeBytes(ctx, obj.Name)
		if err != nil {
			return nil, err
		}
		obj.SizeBytes = size
	}
	return obj, nil
}

func cubecowVolumeToSnapshotObject(ctx context.Context, manager cowVolumeManager, volume *cowVolume) (*CowSnapshotObject, error) {
	if volume == nil {
		return nil, fmt.Errorf("cubecow volume is nil")
	}
	return resolveCowObject(ctx, manager, volume.VolumeName, volume.Kind, volume.Gen)
}

func cubecowVolumeToSnapshotObjectWithoutActivation(ctx context.Context, manager cowVolumeManager, volume *cowVolume) (*CowSnapshotObject, error) {
	if volume == nil {
		return nil, fmt.Errorf("cubecow volume is nil")
	}
	size, err := manager.GetSizeBytes(ctx, volume.VolumeName)
	if err != nil {
		return nil, err
	}
	return &CowSnapshotObject{
		Name:      volume.VolumeName,
		Kind:      volume.Kind,
		DevPath:   volume.FilePath,
		SizeBytes: size,
		Gen:       volume.Gen,
	}, nil
}

func resolveCowObject(ctx context.Context, manager cowVolumeManager, name, kind string, gen uint32) (*CowSnapshotObject, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("cubecow object name is required")
	}
	devPath, err := manager.ResolveDevPath(ctx, name, kind)
	if err != nil {
		return nil, err
	}
	size, err := manager.GetSizeBytes(ctx, name)
	if err != nil {
		return nil, err
	}
	return &CowSnapshotObject{
		Name:      name,
		Kind:      kind,
		DevPath:   devPath,
		SizeBytes: size,
		Gen:       gen,
	}, nil
}

func cowTemplateBuildRootfsName(templateID string) string {
	return fmt.Sprintf("tpl-%s-build-rootfs", templateID)
}

func cowTemplateRootfsName(templateID string) string {
	return fmt.Sprintf("tpl-%s-rootfs", templateID)
}

func cowTemplateMemoryName(templateID string) string {
	return fmt.Sprintf("tpl-%s-memory", templateID)
}

func normalizeCowKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case cowKindVolume:
		return cowKindVolume, nil
	case cowKindSnapshot:
		return cowKindSnapshot, nil
	default:
		return "", fmt.Errorf("unsupported cubecow kind %q", kind)
	}
}

// normalizeCowKindForCleanup prefers snapshot when the object name is a
// sealed *-snap (template memory-snap, metadata snap). Empty kind + role
// memory would otherwise default to volume and miss the activated snap.
func normalizeCowKindForCleanup(kind, role, name string) (string, error) {
	if strings.TrimSpace(kind) == "" && strings.HasSuffix(strings.TrimSpace(name), "-snap") {
		return cowKindSnapshot, nil
	}
	return normalizeCowKindForRole(kind, role)
}

// normalizeCowKindForRole resolves a cubecow kind, defaulting an empty/blank
// kind from the object role instead of failing. CubeMaster catalog entries do
// not always carry an explicit kind; defaulting keeps cleanup from aborting,
// and DeleteByKind auto-recovers if the guessed kind is wrong.
func normalizeCowKindForRole(kind, role string) (string, error) {
	if strings.TrimSpace(kind) == "" {
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "rootfs":
			return cowKindSnapshot, nil
		case "metadata":
			return cowKindSnapshot, nil
		default:
			// memory / build_rootfs / unknown -> volume (matches rollback path)
			return cowKindVolume, nil
		}
	}
	return normalizeCowKind(kind)
}

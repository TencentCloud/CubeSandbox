// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
)

const (
	CowKindVolume   = cowKindVolume
	CowKindSnapshot = cowKindSnapshot
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

func GetSandboxRootfsForSnapshot(ctx context.Context, sandboxID, preferredVolumeName string) (*CowSnapshotObject, error) {
	if localStorage == nil {
		return nil, fmt.Errorf("storage is not initialized")
	}
	if !localStorage.useCowStorage() {
		return nil, fmt.Errorf("storage backend is not cubecow")
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	rootfs, err := selectSnapshotRootfs(info, preferredVolumeName)
	if err != nil {
		return nil, err
	}
	return backendFileInfoToSnapshotObject(ctx, localStorage.cowManager, rootfs)
}

func CommitTemplateRootfs(ctx context.Context, source *CowSnapshotObject, templateID string) (*CowSnapshotObject, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	if source == nil || source.Name == "" {
		return nil, fmt.Errorf("source rootfs is required")
	}
	volume, err := manager.CommitTemplateRootfs(ctx, source.Name, templateID)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObjectWithoutActivation(ctx, manager, volume)
}

func CreateTemplateRootfsFromBuild(ctx context.Context, templateID string) (*CowSnapshotObject, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	volume, err := manager.CommitTemplateRootfs(ctx, cowTemplateBuildRootfsName(templateID), templateID)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObjectWithoutActivation(ctx, manager, volume)
}

func CreateTemplateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*CowSnapshotObject, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	volume, err := manager.CreateMemoryVolume(ctx, templateID, sizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, manager, volume)
}

// CommitTemplateMemoryFromBase clones an existing memory object (typically the
// memory blob backing the sandbox's current base snapshot/template) into the
// canonical template memory name for templateID. The resulting object is a
// cubecow snapshot whose content starts as an exact reflink-shared copy of the
// source, making it a valid base for the hypervisor's incremental
// (pagemap_anon) memory snapshot, which only writes CoW anonymous pages and
// relies on the rest of memory being preserved from the base file.
func CommitTemplateMemoryFromBase(ctx context.Context, source *CowSnapshotObject, templateID string, sizeBytes uint64) (*CowSnapshotObject, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	if source == nil || strings.TrimSpace(source.Name) == "" {
		return nil, fmt.Errorf("source memory object is required")
	}
	volume, err := manager.CommitTemplateMemory(ctx, source.Name, templateID, sizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, manager, volume)
}

func DefaultTemplateObjectRefs(templateID string) []CowObjectRef {
	return []CowObjectRef{
		{Name: cowTemplateRootfsName(templateID), Kind: cowKindSnapshot, Role: "rootfs"},
		{Name: cowTemplateMemoryName(templateID), Kind: cowKindVolume, Role: "memory"},
		{Name: cowTemplateBuildRootfsName(templateID), Kind: cowKindVolume, Role: "build_rootfs"},
	}
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
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	rootfs, err := resolveCowObject(ctx, manager, rootfsVol, cowKindSnapshot, 0)
	if err != nil {
		return nil, err
	}
	normalizedMemoryKind, err := resolveRollbackMemoryKind(memoryKind)
	if err != nil {
		return nil, err
	}
	memory, err := resolveCowObject(ctx, manager, memoryVol, normalizedMemoryKind, 0)
	if err != nil {
		return nil, err
	}
	return &CowRollbackSnapshotRefs{Rootfs: rootfs, Memory: memory}, nil
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
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	volume, err := manager.RollbackDeriveNewGen(ctx, sandboxID, snapshotRootfsVol, newGen, desiredSizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, manager, volume)
}

func PersistSandboxRootfsAfterRollback(ctx context.Context, sandboxID string, rootfs *CowSnapshotObject) error {
	if localStorage == nil {
		return fmt.Errorf("storage is not initialized")
	}
	if rootfs == nil || rootfs.Name == "" {
		return fmt.Errorf("rollback rootfs is required")
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		return err
	}
	current, err := selectSnapshotRootfs(info, rootfs.MountName)
	if err != nil {
		return err
	}
	current.VolumeName = rootfs.Name
	current.Kind = rootfs.Kind
	current.Gen = rootfs.Gen
	current.FilePath = rootfs.DevPath
	current.SizeLimit = int64(rootfs.SizeBytes)
	info.UpdateAt = time.Now()
	return localStorage.writeBackendFileInfo(ctx, sandboxID, info)
}

func DeleteCowObject(ctx context.Context, name, kind string) error {
	manager, err := requireCowManager()
	if err != nil {
		return err
	}
	return manager.DeleteByKind(ctx, name, kind)
}

func DeactivateCowObject(ctx context.Context, name, kind string) error {
	manager, err := requireCowManager()
	if err != nil {
		return err
	}
	return manager.DeactivateByKind(ctx, name, kind)
}

func ResolveCowDevPath(ctx context.Context, name, kind string) (string, error) {
	manager, err := requireCowManager()
	if err != nil {
		return "", err
	}
	normalizedKind, err := normalizeCowKind(kind)
	if err != nil {
		return "", err
	}
	return manager.ResolveDevPath(ctx, name, normalizedKind)
}

func CleanupCowTemplateObjects(ctx context.Context, refs []CowObjectRef) error {
	manager, err := requireCowManager()
	if err != nil {
		return err
	}
	// FIX-3: best-effort cleanup. A single object failing (e.g. a missing or
	// kind-mismatched entry) must not abort cleanup of the remaining objects,
	// otherwise sibling cubecow objects leak. Errors are aggregated; the caller
	// (CubeMaster) keeps template metadata on failure and retries, and each
	// DeleteByKind is idempotent (NotFound -> success).
	var cleanupErr error
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" {
			continue
		}
		kind, err := normalizeCowKindForRole(ref.Kind, ref.Role)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup cubecow object %q: %w", ref.Name, err))
			continue
		}
		if err := manager.DeleteByKind(ctx, ref.Name, kind); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup cubecow object %q: %w", ref.Name, err))
			continue
		}
	}
	return cleanupErr
}

func InspectCowObjects(ctx context.Context, refs []CowObjectRef) ([]CowObjectStatus, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	statuses := make([]CowObjectStatus, 0, len(refs))
	for _, ref := range refs {
		status := CowObjectStatus{
			Name: ref.Name,
			Kind: ref.Kind,
			Role: ref.Role,
		}
		if strings.TrimSpace(ref.Name) == "" {
			statuses = append(statuses, status)
			continue
		}
		kind, err := normalizeCowKind(ref.Kind)
		if err != nil {
			return nil, fmt.Errorf("inspect cubecow object %q: %w", ref.Name, err)
		}
		status.Kind = kind
		info, err := manager.GetVolumeInfo(ctx, ref.Name)
		if err != nil {
			if isCowSemantic(err, cubecow.SemNotFound) {
				statuses = append(statuses, status)
				continue
			}
			return nil, fmt.Errorf("inspect cubecow object %q: %w", ref.Name, err)
		}
		if info == nil {
			statuses = append(statuses, status)
			continue
		}
		status.Exists = true
		status.DevicePath = info.DevicePath
		status.SizeBytes = info.SizeBytes
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func GetCowMetrics(ctx context.Context) (map[string]uint64, error) {
	manager, err := requireCowManager()
	if err != nil {
		return nil, err
	}
	metrics, err := manager.GetMetrics(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range cowMetricKeys {
		if _, ok := metrics[key]; !ok {
			return nil, fmt.Errorf("cubecow metric %q is missing", key)
		}
	}
	return metrics, nil
}

func requireCowManager() (cowVolumeManager, error) {
	if localStorage == nil {
		return nil, fmt.Errorf("storage is not initialized")
	}
	if !localStorage.useCowStorage() {
		return nil, fmt.Errorf("storage backend is not cubecow")
	}
	if err := localStorage.ensureCowManager(); err != nil {
		return nil, err
	}
	if localStorage.cowManager == nil {
		return nil, fmt.Errorf("cubecow manager not initialized")
	}
	return localStorage.cowManager, nil
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

// normalizeCowKindForRole resolves a cubecow kind, defaulting an empty/blank
// kind from the object role instead of failing. CubeMaster catalog entries do
// not always carry an explicit kind; defaulting keeps cleanup from aborting,
// and DeleteByKind auto-recovers if the guessed kind is wrong.
func normalizeCowKindForRole(kind, role string) (string, error) {
	if strings.TrimSpace(kind) == "" {
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "rootfs":
			return cowKindSnapshot, nil
		default:
			// memory / build_rootfs / unknown -> volume (matches rollback path)
			return cowKindVolume, nil
		}
	}
	return normalizeCowKind(kind)
}

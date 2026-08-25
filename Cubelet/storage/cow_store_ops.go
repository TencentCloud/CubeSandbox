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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// volumeDeleteDeferredLog is the stable prefix leak detection greps for.
// A volume Cubelet failed to delete is not a leak: the backend collects it.
const volumeDeleteDeferredLog = "volume delete deferred"

func warnVolumeDeleteDeferred(ctx context.Context, name, kind string, err error) {
	if err == nil {
		return
	}
	log.G(ctx).Warnf("%s: name=%s kind=%s: %v", volumeDeleteDeferredLog, name, kind, err)
}

func isCowVolumeKind(kind string) bool {
	normalized, err := normalizeCowKind(kind)
	return err == nil && normalized == cowKindVolume
}

// requireCowStore returns the default (XFS) [cow.Store].
func requireCowStore() (cow.Store, error) {
	return requireCowStoreFor(cow.BackendXFS)
}

// requireCowStoreFor returns the [cow.Store] for the given backend type
// (request `type`: xfs｜s3). Empty / unknown aliases are normalized via
// [cow.NormalizeBackend].
func requireCowStoreFor(backend string) (cow.Store, error) {
	if localStorage == nil {
		return nil, fmt.Errorf("storage is not initialized")
	}
	if !localStorage.useCowStorage() {
		return nil, fmt.Errorf("storage backend is not cubecow")
	}
	if err := localStorage.ensureCowManager(); err != nil {
		return nil, err
	}
	store, err := StoreFor(backend)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("cow store is not initialized")
	}
	return store, nil
}

// UploadSnapshot publishes snapshot disks to the remote store
// (cubecow_export_snapshot). XFS is a successful no-op (no remote upload).
func UploadSnapshot(ctx context.Context, backend, snapshotID string) (*cow.RemoteUUIDs, error) {
	uploader, err := requireCowUploader(backend)
	if err != nil {
		return nil, err
	}
	return uploader.Upload(ctx, snapshotID)
}

// UploadTemplateRootfs exports a template's rootfs snapshot, the parent
// layer of every sandbox rootfs derived from it. See
// [S3Cow.UploadTemplateRootfs]. XFS returns an empty uuid and no error.
func UploadTemplateRootfs(ctx context.Context, backend, templateID string) (string, error) {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil {
		return "", err
	}
	if normalized != cow.BackendS3 {
		return "", nil
	}
	store, err := requireCowStoreFor(cow.BackendS3)
	if err != nil {
		return "", err
	}
	s3, ok := store.(*S3Cow)
	if !ok || s3 == nil {
		return "", fmt.Errorf("s3 cow store does not support template rootfs export")
	}
	return s3.UploadTemplateRootfs(ctx, templateID)
}

// SnapshotUploadStatus returns upload state from cubecow export_status
// (NONE→pending, empty/INPROGRESS→running, DONE→ready). XFS is a no-op ready.
func SnapshotUploadStatus(ctx context.Context, backend, snapshotID string) (*cow.RemoteStatus, error) {
	uploader, err := requireCowUploader(backend)
	if err != nil {
		return nil, err
	}
	return uploader.UploadStatus(ctx, snapshotID)
}

// ActivateSnapshot opens a local snapshot's cubecow objects on the Store
// for backend. Missing objects fail; this does not fetch from remote
// (use FetchSnapshot for that). Does not start a sandbox. Callers decide
// when to activate versus clone-to-volume first.
func ActivateSnapshot(ctx context.Context, backend, snapshotID string) error {
	activator, err := requireCowActivator(backend)
	if err != nil {
		return err
	}
	return activator.Activate(ctx, snapshotID)
}

// RestoreSnapshot is the previous name of [ActivateSnapshot].
func RestoreSnapshot(ctx context.Context, backend, snapshotID string) error {
	return ActivateSnapshot(ctx, backend, snapshotID)
}

type noopUploader struct{}

func (noopUploader) Upload(ctx context.Context, snapshotID string) (*cow.RemoteUUIDs, error) {
	_ = ctx
	_ = snapshotID
	return &cow.RemoteUUIDs{}, nil
}

func (noopUploader) UploadStatus(ctx context.Context, snapshotID string) (*cow.RemoteStatus, error) {
	_ = ctx
	id := strings.TrimSpace(snapshotID)
	return &cow.RemoteStatus{SnapshotID: id, State: cow.RemoteStateReady, Message: "xfs has no remote upload"}, nil
}

func requireCowUploader(backend string) (cow.Uploader, error) {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil {
		return nil, err
	}
	if normalized != cow.BackendS3 {
		return noopUploader{}, nil
	}
	store, err := requireCowStoreFor(cow.BackendS3)
	if err != nil {
		return nil, err
	}
	uploader, ok := store.(cow.Uploader)
	if !ok || uploader == nil {
		return nil, fmt.Errorf("s3 cow store does not implement upload")
	}
	return uploader, nil
}

func requireCowActivator(backend string) (cow.Activator, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	if activator, ok := store.(cow.Activator); ok && activator != nil {
		return activator, nil
	}
	return storeActivator{store: store, backend: backend}, nil
}

type storeActivator struct {
	store   cow.Store
	backend string
}

func (a storeActivator) Activate(ctx context.Context, snapshotID string) error {
	return activateStoreObjects(ctx, a.store, a.backend, snapshotID)
}

// StoreFor returns the live XFS or S3 [cow.Store] for backend (request `type`).
// XFS is ready after plugin init; S3 returns [ErrS3NotReady] until the
// background s3lvol init loop publishes the S3 handle.
func StoreFor(backend string) (cow.Store, error) {
	if localStorage == nil {
		return nil, fmt.Errorf("storage is not initialized")
	}
	return localStorage.storeForBackend(backend)
}

// GetSandboxRootfs resolves the live sandbox rootfs CoW object on the default Store.
func GetSandboxRootfs(ctx context.Context, sandboxID, preferredVolumeName string) (*CowSnapshotObject, error) {
	return GetSandboxRootfsFor(ctx, cow.BackendXFS, sandboxID, preferredVolumeName)
}

// GetSandboxRootfsFor is [GetSandboxRootfs] on the Store selected by backend.
func GetSandboxRootfsFor(ctx context.Context, backend, sandboxID, preferredVolumeName string) (*CowSnapshotObject, error) {
	if localStorage == nil {
		return nil, fmt.Errorf("storage is not initialized")
	}
	if !localStorage.useCowStorage() {
		return nil, fmt.Errorf("storage backend is not cubecow")
	}
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	rootfs, err := selectSnapshotRootfs(info, preferredVolumeName)
	if err != nil {
		return nil, err
	}
	return backendFileInfoToSnapshotObject(ctx, store, rootfs)
}

// CommitRootfs commits source rootfs into tpl-<id>-rootfs on the default (XFS) Store.
func CommitRootfs(ctx context.Context, source *CowSnapshotObject, id string) (*CowSnapshotObject, error) {
	return CommitRootfsFor(ctx, cow.BackendXFS, source, id)
}

// CommitRootfsFor is [CommitRootfs] on the Store selected by backend (request `type`).
func CommitRootfsFor(ctx context.Context, backend string, source *CowSnapshotObject, id string) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	if source == nil || source.Name == "" {
		return nil, fmt.Errorf("source rootfs is required")
	}
	volume, err := store.CommitTemplateRootfs(ctx, source.Name, id)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObjectWithoutActivation(ctx, store, volume)
}

// CommitRootfsFromBuild commits tpl-<id>-build-rootfs into tpl-<id>-rootfs.
func CommitRootfsFromBuild(ctx context.Context, id string) (*CowSnapshotObject, error) {
	return CommitRootfsFromBuildFor(ctx, cow.BackendXFS, id)
}

// CommitRootfsFromBuildFor is [CommitRootfsFromBuild] on the Store for backend.
func CommitRootfsFromBuildFor(ctx context.Context, backend, id string) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	volume, err := store.CommitTemplateRootfs(ctx, cowTemplateBuildRootfsName(id), id)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObjectWithoutActivation(ctx, store, volume)
}

// CreateMemoryVolume creates empty tpl-<id>-memory via the default (XFS) Store.
func CreateMemoryVolume(ctx context.Context, id string, sizeBytes uint64) (*CowSnapshotObject, error) {
	return CreateMemoryVolumeFor(ctx, cow.BackendXFS, id, sizeBytes)
}

// CreateMemoryVolumeFor is [CreateMemoryVolume] on the Store for backend.
func CreateMemoryVolumeFor(ctx context.Context, backend, id string, sizeBytes uint64) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	volume, err := store.CreateMemoryVolume(ctx, id, sizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, store, volume)
}

// CommitMemoryFromBase clones source memory into tpl-<id>-memory via the default Store.
func CommitMemoryFromBase(ctx context.Context, source *CowSnapshotObject, id string, sizeBytes uint64) (*CowSnapshotObject, error) {
	return CommitMemoryFromBaseFor(ctx, cow.BackendXFS, source, id, sizeBytes)
}

// CommitMemoryFromBaseFor is [CommitMemoryFromBase] on the Store for backend.
func CommitMemoryFromBaseFor(ctx context.Context, backend string, source *CowSnapshotObject, id string, sizeBytes uint64) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	if source == nil || strings.TrimSpace(source.Name) == "" {
		return nil, fmt.Errorf("source memory object is required")
	}
	volume, err := store.CommitTemplateMemory(ctx, source.Name, id, sizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, store, volume)
}

// DeleteObject deletes a CoW volume/snapshot by name and kind on the default Store.
func DeleteObject(ctx context.Context, name, kind string) error {
	return DeleteObjectFor(ctx, cow.BackendXFS, name, kind)
}

// DeleteObjectFor is [DeleteObject] on the Store for backend.
func DeleteObjectFor(ctx context.Context, backend, name, kind string) error {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return err
	}
	return store.DeleteByKind(ctx, name, kind)
}

// DeactivateObject deactivates a CoW volume/snapshot on the default Store.
func DeactivateObject(ctx context.Context, name, kind string) error {
	return DeactivateObjectFor(ctx, cow.BackendXFS, name, kind)
}

// DeactivateObjectFor is [DeactivateObject] on the Store for backend.
func DeactivateObjectFor(ctx context.Context, backend, name, kind string) error {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return err
	}
	return store.DeactivateByKind(ctx, name, kind)
}

// ResolveObjectPath resolves a CoW object to a device/file path on the default Store.
func ResolveObjectPath(ctx context.Context, name, kind string) (string, error) {
	return ResolveObjectPathFor(ctx, cow.BackendXFS, name, kind)
}

// ResolveObjectPathFor is [ResolveObjectPath] on the Store for backend.
func ResolveObjectPathFor(ctx context.Context, backend, name, kind string) (string, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return "", err
	}
	normalizedKind, err := normalizeCowKind(kind)
	if err != nil {
		return "", err
	}
	return store.ResolveDevPath(ctx, name, normalizedKind)
}

// CleanupObjects best-effort deletes the given CoW object refs on the default Store.
func CleanupObjects(ctx context.Context, refs []CowObjectRef) error {
	return CleanupObjectsFor(ctx, cow.BackendXFS, refs)
}

// CleanupObjectsFor is [CleanupObjects] on the Store selected by backend.
//
// The rootfs object goes first and a failure on it stops the sweep. rootfs is
// what proves the package exists — every other object name can be re-derived
// from the package id, but only while the package is still whole. Deleting
// memory and metadata under a rootfs that refused to go (an exported snapshot
// stays undeletable until its export is released) turns a retryable failure
// into a half package that no retry can repair.
func CleanupObjectsFor(ctx context.Context, backend string, refs []CowObjectRef) error {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return err
	}
	var cleanupErr error
	for _, ref := range orderCleanupRefsRootfsFirst(refs) {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		kind, err := normalizeCowKindForCleanup(ref.Kind, ref.Role, name)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup cubecow object %q: %w", name, err))
			continue
		}
		// Tear down NVMe/host activation first so an in-use memory-snap
		// can be deleted. Deactivate failure is not fatal: cubecow
		// delete_snapshot also deactivates, and NotFound is success.
		_ = store.DeactivateByKind(ctx, name, kind)
		if err := store.DeleteByKind(ctx, name, kind); err != nil {
			if isCowVolumeKind(kind) {
				warnVolumeDeleteDeferred(ctx, name, kind, err)
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup cubecow object %q: %w", name, err))
			if isRootfsCleanupRole(ref.Role) {
				return cleanupErr
			}
			continue
		}
		if isXFSCleanupBackend(backend) {
			if err := removeLegacyXfsCowObjectIfGone(ctx, name); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup legacy cubecow object %q: %w", name, err))
			}
		}
	}
	return cleanupErr
}

// orderCleanupRefsRootfsFirst hoists rootfs refs to the front, leaving the
// relative order of everything else untouched.
func orderCleanupRefsRootfsFirst(refs []CowObjectRef) []CowObjectRef {
	ordered := make([]CowObjectRef, 0, len(refs))
	for _, ref := range refs {
		if isRootfsCleanupRole(ref.Role) {
			ordered = append(ordered, ref)
		}
	}
	if len(ordered) == 0 || len(ordered) == len(refs) {
		return refs
	}
	for _, ref := range refs {
		if !isRootfsCleanupRole(ref.Role) {
			ordered = append(ordered, ref)
		}
	}
	return ordered
}

// isRootfsCleanupRole matches the package rootfs only. build_rootfs is a
// template build leftover, not the package identity, so it does not gate the
// rest of the sweep.
func isRootfsCleanupRole(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "rootfs")
}

// InspectObjects reports existence/device path for CoW object refs on the default Store.
func InspectObjects(ctx context.Context, refs []CowObjectRef) ([]CowObjectStatus, error) {
	return InspectObjectsFor(ctx, cow.BackendXFS, refs)
}

// InspectObjectsFor is [InspectObjects] on the Store selected by backend.
func InspectObjectsFor(ctx context.Context, backend string, refs []CowObjectRef) ([]CowObjectStatus, error) {
	store, err := requireCowStoreFor(backend)
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
		info, err := store.GetVolumeInfo(ctx, ref.Name)
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

// ObjectMetrics returns metrics from the default (XFS) [cow.Store].
func ObjectMetrics(ctx context.Context) (map[string]uint64, error) {
	return ObjectMetricsFor(ctx, cow.BackendXFS)
}

// ObjectMetricsFor returns metrics from the Store selected by backend.
func ObjectMetricsFor(ctx context.Context, backend string) (map[string]uint64, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	metrics, err := store.GetMetrics(ctx)
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

// ResolveRollbackRefs resolves rootfs/memory objects for rollback on the default Store.
func ResolveRollbackRefs(ctx context.Context, rootfsVol, memoryVol, memoryKind string) (*CowRollbackSnapshotRefs, error) {
	return ResolveRollbackRefsFor(ctx, cow.BackendXFS, rootfsVol, memoryVol, memoryKind)
}

// ResolveRollbackRefsFor is [ResolveRollbackRefs] on the Store selected by backend.
func ResolveRollbackRefsFor(ctx context.Context, backend, rootfsVol, memoryVol, memoryKind string) (*CowRollbackSnapshotRefs, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	rootfs, err := resolveCowObject(ctx, store, rootfsVol, cowKindSnapshot, 0)
	if err != nil {
		return nil, err
	}
	normalizedMemoryKind, err := resolveRollbackMemoryKind(memoryKind)
	if err != nil {
		return nil, err
	}
	memory, err := resolveCowObject(ctx, store, memoryVol, normalizedMemoryKind, 0)
	if err != nil {
		return nil, err
	}
	return &CowRollbackSnapshotRefs{Rootfs: rootfs, Memory: memory}, nil
}

// DeriveRollbackRootfs creates sb-<id>-rootfs-genN from a snapshot rootfs on the default Store.
func DeriveRollbackRootfs(ctx context.Context, sandboxID, snapshotRootfsVol string, newGen uint32, desiredSizeBytes uint64) (*CowSnapshotObject, error) {
	return DeriveRollbackRootfsFor(ctx, cow.BackendXFS, sandboxID, snapshotRootfsVol, newGen, desiredSizeBytes)
}

// DeriveRollbackRootfsFor is [DeriveRollbackRootfs] on the Store selected by backend.
func DeriveRollbackRootfsFor(ctx context.Context, backend, sandboxID, snapshotRootfsVol string, newGen uint32, desiredSizeBytes uint64) (*CowSnapshotObject, error) {
	store, err := requireCowStoreFor(backend)
	if err != nil {
		return nil, err
	}
	volume, err := store.RollbackDeriveNewGen(ctx, sandboxID, snapshotRootfsVol, newGen, desiredSizeBytes)
	if err != nil {
		return nil, err
	}
	return cubecowVolumeToSnapshotObject(ctx, store, volume)
}

// PersistSandboxRootfs records the live sandbox rootfs after rollback.
func PersistSandboxRootfs(ctx context.Context, sandboxID string, rootfs *CowSnapshotObject) error {
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

// refuseS3PackageObjectDelete reports whether a sandbox-lifecycle delete must
// leave this object alone. On S3 a snapshot lives in s3lvol and is shared by
// whoever restores from the package that owns it, so only the snapshot delete
// flow may remove one; a sandbox operation deleting it would take the package
// out from under other sandboxes.
//
// XFS is exempt on purpose: there a "snapshot" is a reflink file that the
// sandbox itself owns, and the existing lifecycle is unchanged.
func refuseS3PackageObjectDelete(backend, name, sandboxID string) bool {
	if !isS3CatalogBackend(backend) {
		return false
	}
	name = strings.TrimSpace(name)
	sandboxID = strings.TrimSpace(sandboxID)
	if name == "" || sandboxID == "" {
		return false
	}
	return !sandboxOwnsS3Object(name, sandboxID)
}

// sandboxOwnsS3Object reports whether name is one of the disks this sandbox
// mints for itself: its rootfs generations and imported memory
// ([SandboxRootfsName] / [SandboxMemoryName]), its private metadata disk, and
// the metadata copy a rollback reads. Anything else on an S3 node belongs to a
// package.
//
// The cubecow kind cannot answer this — a sandbox rootfs generation is created
// through create_snapshot and recorded as kind=snapshot — so ownership is read
// off the name. It is an exact match against what the minting helpers produce
// rather than a substring test, so an object that merely mentions the sandbox
// id cannot pass for one of its own.
func sandboxOwnsS3Object(name, sandboxID string) bool {
	if strings.HasPrefix(name, SandboxObjectNamePrefix(sandboxID)) {
		return true
	}
	return name == S3MetadataVolumeName(sandboxID) ||
		name == S3MetadataVolumeName(RollbackMetadataOwnerID(sandboxID))
}

// ReleaseRollbackReplacedVolumes deletes the sandbox volumes rollback
// replaced: the previous rootfs generation, and any imported memory disk a
// cross-node restore left behind. Package snapshots are not touched — they
// outlive the sandbox. Missing storage metadata is ignored so a unit test
// without BoltDB can still assert the cubecow deletes.
func ReleaseRollbackReplacedVolumes(ctx context.Context, backend, sandboxID string, oldRootfs *CowSnapshotObject) error {
	var errs error
	if oldRootfs != nil {
		if name := strings.TrimSpace(oldRootfs.Name); name != "" {
			kind := strings.TrimSpace(oldRootfs.Kind)
			if kind == "" {
				kind = cowKindVolume
			}
			if refuseS3PackageObjectDelete(backend, name, sandboxID) {
				// The sandbox was running straight off a package object.
				// Removing one is the snapshot delete flow's job, never a
				// sandbox operation's, so report it instead of destroying
				// something another sandbox may still restore from.
				errs = errors.Join(errs, fmt.Errorf("refuse to delete replaced rootfs %s kind %s: not minted for sandbox %s", name, kind, sandboxID))
			} else {
				_ = DeactivateObjectFor(ctx, backend, name, kind)
				if err := DeleteObjectFor(ctx, backend, name, kind); err != nil {
					warnVolumeDeleteDeferred(ctx, name, kind, err)
				}
			}
		}
	}
	if err := releaseImportedMemoryAfterRollback(ctx, backend, sandboxID); err != nil {
		errs = errors.Join(errs, err)
	}
	return errs
}

func releaseImportedMemoryAfterRollback(ctx context.Context, backend, sandboxID string) error {
	if localStorage == nil || localStorage.db == nil {
		return nil
	}
	info, err := localStorage.readBackendFileInfo(ctx, sandboxID)
	if err != nil {
		return nil
	}
	name := strings.TrimSpace(info.ImportedMemoryVol)
	if name == "" {
		return nil
	}
	_ = DeactivateObjectFor(ctx, backend, name, cowKindVolume)
	if err := DeleteObjectFor(ctx, backend, name, cowKindVolume); err != nil {
		warnVolumeDeleteDeferred(ctx, name, cowKindVolume, err)
	}
	info.ImportedMemoryVol = ""
	info.UpdateAt = time.Now()
	if err := localStorage.writeBackendFileInfo(ctx, sandboxID, info); err != nil {
		return fmt.Errorf("clear imported memory %s after rollback: %w", name, err)
	}
	return nil
}

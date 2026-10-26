// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	// S3MetadataBaseVolumeName is the per-node, local-only cubecow volume
	// that Cubelet formats as ext4 during S3 init.
	S3MetadataBaseVolumeName = "cubelet-s3-metadata-base"
	// S3MetadataBaseSnapshotName is the RO snapshot of the base volume.
	// Package metadata disks are writable volumes cloned from this snap
	// (or from a parent package metadata volume via snap→clone).
	S3MetadataBaseSnapshotName = "cubelet-s3-metadata-base-snap"
	s3MetadataBaseSizeBytes    = 8 << 20
	s3MetadataVolumePrefix     = "s3-meta-"
	s3MetadataBucket           = "s3-metadata/v1"
	s3MetadataStateKey         = "state"
	// rollbackMetadataOwnerSuffix distinguishes the metadata copy a rollback
	// reads from the sandbox's own metadata disk, which keeps its plain id.
	rollbackMetadataOwnerSuffix = "-rollback"
	rollbackMetadataDirName     = "rollback-metadata"
)

type s3MetadataBaseRecord struct {
	Name         string `json:"name"`
	SnapshotName string `json:"snapshot_name,omitempty"`
	SizeBytes    uint64 `json:"size_bytes"`
	Formatted    bool   `json:"formatted"`
}

type s3MetadataDerivedRecord struct {
	SnapshotID string `json:"snapshot_id"`
	VolName    string `json:"vol_name"`
	MountPath  string `json:"mount_path,omitempty"`
}

type s3MetadataState struct {
	Base    s3MetadataBaseRecord               `json:"base"`
	Derived map[string]s3MetadataDerivedRecord `json:"derived,omitempty"`
}

type s3MetadataKV interface {
	Get() ([]byte, error)
	Set([]byte) error
}

var (
	formatS3MetadataBaseDevice = formatS3MetadataBaseDeviceImpl
	mountS3MetadataDevice      = mountS3MetadataDeviceImpl
	unmountS3MetadataPath      = unmountS3MetadataPathImpl
	s3MetadataIsMounted        = s3MetadataIsMountedImpl

	s3MetadataMu     sync.Mutex
	s3MetadataMounts = map[string]string{} // snapshotID → current mount path
	testS3MetadataKV s3MetadataKV
)

// S3MetadataVolumeName is the writable cubecow volume for one template /
// pause / snapshot package metadata disk (cloned from the base snapshot).
func S3MetadataVolumeName(snapshotID string) string {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return ""
	}
	return s3MetadataVolumePrefix + id
}

// S3MetadataSnapshotName is the RO sealed snapshot of the package metadata
// volume. Only this name is exported／fetched across nodes.
func S3MetadataSnapshotName(snapshotID string) string {
	name := S3MetadataVolumeName(snapshotID)
	if name == "" {
		return ""
	}
	return name + "-snap"
}

// IsS3MetadataBaseName reports the node-local metadata base volume or its
// RO snapshot. Neither must be exported or fetched; they stay on this Cubelet.
func IsS3MetadataBaseName(name string) bool {
	n := strings.TrimSpace(name)
	return n == S3MetadataBaseVolumeName || n == S3MetadataBaseSnapshotName
}

// S3MetadataCatalogVol is the catalog metadata_vol for an S3 package.
// Before finalize this is the RW work volume; FinalizeS3PackageSnapshots
// rewrites it to S3MetadataSnapshotName for export.
func S3MetadataCatalogVol(backend, snapshotID string) string {
	if !isS3CatalogBackend(backend) {
		return ""
	}
	return S3MetadataVolumeName(snapshotID)
}

// S3MetadataCatalogKind is volume while metadata is still being written;
// FinalizeS3PackageSnapshots switches the catalog entry to snapshot.
func S3MetadataCatalogKind(backend string) string {
	if !isS3CatalogBackend(backend) {
		return ""
	}
	return cowKindVolume
}

func cowObjectPresent(info *cubecow.Volume, err error) (bool, error) {
	if err != nil {
		if isCowSemantic(err, cubecow.SemNotFound) {
			return false, nil
		}
		return false, err
	}
	return info != nil, nil
}

func requireS3Cow() (*S3Cow, error) {
	s3CowOverrideMu.Lock()
	override := s3CowOverride
	s3CowOverrideMu.Unlock()
	if override != nil {
		return override, nil
	}
	if localStorage == nil || localStorage.s3CowManager == nil {
		return nil, ErrS3NotReady
	}
	store, ok := localStorage.s3CowManager.(*S3Cow)
	if !ok || store == nil {
		return nil, fmt.Errorf("s3 cow store is not *S3Cow")
	}
	return store, nil
}

// EnsureS3MetadataReady creates (or reconciles) the node-local 8MiB metadata
// base. Nothing else is done at startup: every package disk is derived and
// mounted by the request that reads it, and a restarted Cubelet has no way to
// tell a leftover from a disk a live sandbox is running on.
func EnsureS3MetadataReady(ctx context.Context) error {
	return EnsureS3MetadataBase(ctx)
}

// EnsureS3MetadataBase creates the 8MiB base volume and mkfs.ext4's it on
// first use, then snapshots it and deactivates the volume (no further host
// IO). Restart of an older Cubelet that left the base activated is reconciled
// on this path. Missing volumes are recreated.
func EnsureS3MetadataBase(ctx context.Context) error {
	store, err := requireS3Cow()
	if err != nil {
		return err
	}

	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()

	state, err := loadS3MetadataState()
	if err != nil {
		return err
	}
	name := strings.TrimSpace(state.Base.Name)
	if name == "" {
		name = S3MetadataBaseVolumeName
	}
	if name != S3MetadataBaseVolumeName {
		CubeLog.Warnf("s3 metadata base using persisted name %q", name)
	}

	info, infoErr := store.GetVolumeInfo(ctx, name)
	if isCowSemantic(infoErr, cubecow.ErrClosed) {
		CubeLog.Warnf("s3 metadata base skipped: s3 cubecow engine is closed")
		return nil
	}
	exists, err := cowObjectPresent(info, infoErr)
	if err != nil {
		return fmt.Errorf("lookup s3 metadata base %s: %w", name, err)
	}
	if exists {
		state.Base.Name = name
		state.Base.SizeBytes = s3MetadataBaseSizeBytes
		if info != nil && info.SizeBytes > 0 {
			state.Base.SizeBytes = info.SizeBytes
		}
		state.Base.Formatted = true
		if err := ensureS3MetadataBaseSnapshotLocked(ctx, store, &state.Base); err != nil {
			return err
		}
		if err := saveS3MetadataState(state); err != nil {
			return err
		}
		// Idempotent: leftover activate from older Cubelets is torn down here.
		if err := deactivateS3Object(ctx, store, name, cowKindVolume); err != nil {
			CubeLog.Warnf("deactivate s3 metadata base %s: %v", name, err)
		}
		if err := deactivateS3Object(ctx, store, state.Base.SnapshotName, cowKindSnapshot); err != nil {
			CubeLog.Warnf("deactivate s3 metadata base snap %s: %v", state.Base.SnapshotName, err)
		}
		CubeLog.Infof("s3 metadata base %s already present; skip mkfs", name)
		return nil
	}

	devPath, created, err := store.createOrResolveVolumePath(ctx, name, s3MetadataBaseSizeBytes)
	if err != nil {
		return fmt.Errorf("create s3 metadata base %s: %w", name, err)
	}
	devPath, err = requireS3DevicePath(name, "CreateVolume", devPath)
	if err != nil {
		_ = store.DeleteByKind(ctx, name, cowKindVolume)
		return err
	}
	if created || !state.Base.Formatted {
		if err := formatS3MetadataBaseDevice(devPath); err != nil {
			_ = store.DeleteByKind(ctx, name, cowKindVolume)
			return fmt.Errorf("format s3 metadata base %s at %s: %w", name, devPath, err)
		}
	}
	state.Base = s3MetadataBaseRecord{
		Name:      name,
		SizeBytes: s3MetadataBaseSizeBytes,
		Formatted: true,
	}
	if err := ensureS3MetadataBaseSnapshotLocked(ctx, store, &state.Base); err != nil {
		return err
	}
	if err := saveS3MetadataState(state); err != nil {
		return err
	}
	if err := deactivateS3Object(ctx, store, name, cowKindVolume); err != nil {
		CubeLog.Warnf("deactivate s3 metadata base %s: %v", name, err)
	}
	if err := deactivateS3Object(ctx, store, state.Base.SnapshotName, cowKindSnapshot); err != nil {
		CubeLog.Warnf("deactivate s3 metadata base snap %s: %v", state.Base.SnapshotName, err)
	}
	CubeLog.Infof("s3 metadata base %s ready size=%d formatted=%v created=%v snap=%s", name, s3MetadataBaseSizeBytes, true, created, state.Base.SnapshotName)
	return nil
}

func ensureS3MetadataBaseSnapshotLocked(ctx context.Context, store *S3Cow, base *s3MetadataBaseRecord) error {
	if store == nil || base == nil {
		return fmt.Errorf("s3 metadata base snapshot requires store and base record")
	}
	volName := strings.TrimSpace(base.Name)
	if volName == "" {
		volName = S3MetadataBaseVolumeName
		base.Name = volName
	}
	snapName := strings.TrimSpace(base.SnapshotName)
	if snapName == "" {
		snapName = S3MetadataBaseSnapshotName
	}
	info, infoErr := store.GetVolumeInfo(ctx, snapName)
	exists, err := cowObjectPresent(info, infoErr)
	if err != nil {
		return fmt.Errorf("lookup s3 metadata base snapshot %s: %w", snapName, err)
	}
	if !exists {
		if _, err := store.engine.CreateSnapshotFromVolume(volName, snapName, false); err != nil {
			if !isCowSemantic(err, cubecow.SemAlreadyExists) {
				return fmt.Errorf("create s3 metadata base snapshot %s from %s: %w", snapName, volName, err)
			}
		}
	}
	base.SnapshotName = snapName
	return nil
}

// PrepareS3MetadataMount ensures the node-local base volume+snapshot exist,
// clones a writable package metadata volume from the base snapshot, and
// mounts it at mountPath (the designed metadata/ directory). XFS is a no-op.
func PrepareS3MetadataMount(ctx context.Context, backend, snapshotID, mountPath string) error {
	if !isS3CatalogBackend(backend) {
		return nil
	}
	id := strings.TrimSpace(snapshotID)
	mountPath = strings.TrimSpace(mountPath)
	if id == "" || mountPath == "" {
		return fmt.Errorf("s3 metadata snapshot_id and mount path are required")
	}
	if err := EnsureS3MetadataBase(ctx); err != nil {
		return err
	}
	store, err := requireS3Cow()
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("s3 cow store is not initialized")
	}

	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()

	vol, err := store.deriveMetadataSnapshotLocked(ctx, id, "")
	if err != nil {
		return err
	}
	if err := mountS3MetadataLocked(id, vol.FilePath, mountPath); err != nil {
		return err
	}
	return persistDerivedLocked(id, vol.VolumeName, mountPath)
}

// CloneS3MetadataFromParent creates the child package／sandbox metadata disk
// as a snapshot of the parent's metadata volume (template / snapshot /
// pause). If the parent volume is missing, falls back to cloning the
// node-local base. Pause resume uses this with childID=sandboxID so the
// pause package can be deleted afterwards.
func CloneS3MetadataFromParent(ctx context.Context, backend, parentID, childID, mountPath string) error {
	if !isS3CatalogBackend(backend) {
		return nil
	}
	parentID = strings.TrimSpace(parentID)
	childID = strings.TrimSpace(childID)
	mountPath = strings.TrimSpace(mountPath)
	if childID == "" || mountPath == "" {
		return fmt.Errorf("s3 metadata child id and mount path are required")
	}
	if err := EnsureS3MetadataBase(ctx); err != nil {
		return err
	}
	store, err := requireS3Cow()
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("s3 cow store is not initialized")
	}

	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()

	source := ""
	if parentID != "" && parentID != childID {
		for _, candidate := range []string{S3MetadataSnapshotName(parentID), S3MetadataVolumeName(parentID)} {
			if candidate == "" {
				continue
			}
			info, infoErr := store.GetVolumeInfo(ctx, candidate)
			exists, err := cowObjectPresent(info, infoErr)
			if err != nil {
				return fmt.Errorf("lookup parent s3 metadata %s: %w", candidate, err)
			}
			if exists {
				source = candidate
				break
			}
		}
	}
	vol, err := store.deriveMetadataSnapshotLocked(ctx, childID, source)
	if err != nil {
		return err
	}
	if err := mountS3MetadataLocked(childID, vol.FilePath, mountPath); err != nil {
		return err
	}
	return persistDerivedLocked(childID, vol.VolumeName, mountPath)
}

// MountS3MetadataAt mounts the package metadata disk at mountPath.
// IO always goes through a RW volume (s3-meta-<id>). If only the sealed
// snap remains, it is cloned to that volume first — never activate／mount
// the package snap for metadata IO. After Fetch, import_lvol may already
// be a volume under the snap name; that name is used only when cloning
// into s3-meta-<id> is not possible.
func MountS3MetadataAt(ctx context.Context, backend, snapshotID, mountPath string) error {
	if !isS3CatalogBackend(backend) {
		return nil
	}
	id := strings.TrimSpace(snapshotID)
	mountPath = strings.TrimSpace(mountPath)
	if id == "" || mountPath == "" {
		return fmt.Errorf("s3 metadata snapshot_id and mount path are required")
	}
	store, err := requireS3Cow()
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}

	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()

	name, err := store.ensureS3MetadataRWVolumeLocked(ctx, id)
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	devPath, err := store.ResolveDevPath(ctx, name, cowKindVolume)
	if err != nil {
		return fmt.Errorf("resolve s3 metadata volume %s: %w", name, err)
	}
	if err := mountS3MetadataLocked(id, devPath, mountPath); err != nil {
		return err
	}
	return persistDerivedLocked(id, name, mountPath)
}

// RollbackMetadataOwnerID names the metadata copy a rollback reads after the
// sandbox performing it, not after the snapshot it comes from. Two sandboxes
// rolling back to the same snapshot therefore get a disk each instead of
// fighting over one, and destroy knows which one to reclaim.
func RollbackMetadataOwnerID(sandboxID string) string {
	id := strings.TrimSpace(sandboxID)
	if id == "" {
		return ""
	}
	return id + rollbackMetadataOwnerSuffix
}

// MountRollbackSnapshotMetadata clones the snapshot's sealed metadata disk into
// a volume owned by this sandbox and mounts it, returning the directory holding
// snapshot/{config,state}.json. Callers pass that directory to the shim instead
// of the package's own MetaDir, which Finalize left unmounted.
//
// XFS returns an empty path and no error: there the package directory is a
// plain directory that is always readable.
func MountRollbackSnapshotMetadata(ctx context.Context, backend, snapshotID, sandboxID string) (string, error) {
	if !isS3CatalogBackend(backend) {
		return "", nil
	}
	snapshotID = strings.TrimSpace(snapshotID)
	sandboxID = strings.TrimSpace(sandboxID)
	if snapshotID == "" || sandboxID == "" {
		return "", fmt.Errorf("rollback metadata snapshot_id and sandbox_id are required")
	}
	owner := RollbackMetadataOwnerID(sandboxID)
	// A rollback that died before its release ran leaves a clone of some other
	// snapshot's metadata under this name. Reusing it would restore from the
	// wrong config, so drop it and clone again.
	if err := ReleaseRollbackSnapshotMetadata(ctx, backend, sandboxID); err != nil {
		return "", err
	}
	mountPath := rollbackMetadataMountPath(sandboxID)
	if err := CloneS3MetadataFromParent(ctx, backend, snapshotID, owner, mountPath); err != nil {
		return "", err
	}
	return mountPath, nil
}

// ReleaseRollbackSnapshotMetadata unmounts and deletes the copy
// MountRollbackSnapshotMetadata made. The package's sealed metadata snapshot is
// left alone: it belongs to the snapshot, which outlives this sandbox.
func ReleaseRollbackSnapshotMetadata(ctx context.Context, backend, sandboxID string) error {
	owner := RollbackMetadataOwnerID(sandboxID)
	if owner == "" {
		return nil
	}
	if err := ReleaseS3MetadataDerivedVolume(ctx, backend, owner); err != nil {
		return err
	}
	if !isS3CatalogBackend(backend) {
		return nil
	}
	if path := rollbackMetadataMountPath(sandboxID); path != "" {
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove rollback metadata mount %s: %w", path, err)
		}
	}
	return nil
}

func rollbackMetadataMountPath(sandboxID string) string {
	home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, strings.TrimSpace(sandboxID))
	if home == "" {
		return ""
	}
	return filepath.Join(home, rollbackMetadataDirName)
}

// UnmountS3Metadata unmounts a metadata mount point if it is mounted.
func UnmountS3Metadata(mountPath string) error {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		return nil
	}
	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()
	return unmountS3MetadataPathIfMounted(mountPath)
}

// ReleaseS3MetadataVolume unmounts and deletes the derived metadata volume
// together with the package's sealed metadata snapshot. Use it when the
// package itself is going away. The node-local base is never deleted.
func ReleaseS3MetadataVolume(ctx context.Context, backend, snapshotID string) error {
	return releaseS3MetadataVolume(ctx, backend, snapshotID, true)
}

// ReleaseS3MetadataDerivedVolume unmounts and deletes only the RW clone that
// was made to read a package's metadata, leaving the sealed snapshot in place.
// Callers that merely borrow a package (rollback) must use this: the sealed
// snap belongs to the snapshot, not to the operation reading it.
func ReleaseS3MetadataDerivedVolume(ctx context.Context, backend, snapshotID string) error {
	return releaseS3MetadataVolume(ctx, backend, snapshotID, false)
}

func releaseS3MetadataVolume(ctx context.Context, backend, snapshotID string, dropPackageSnapshot bool) error {
	if !isS3CatalogBackend(backend) {
		return nil
	}
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return nil
	}
	name := S3MetadataVolumeName(id)
	if IsS3MetadataBaseName(name) {
		return nil
	}

	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()

	var umountErr error
	if p := s3MetadataMounts[id]; p != "" {
		umountErr = unmountS3MetadataPathIfMounted(p)
	}
	if state, err := loadS3MetadataState(); err == nil && state != nil && state.Derived != nil {
		if rec, ok := state.Derived[id]; ok {
			if p := strings.TrimSpace(rec.MountPath); p != "" {
				if err := unmountS3MetadataPathIfMounted(p); err != nil {
					umountErr = errors.Join(umountErr, err)
				}
			}
		}
	}
	for _, kind := range []string{SnapshotKindPause, SnapshotKindNormal} {
		meta := SnapshotMetaDir(cow.BackendS3, kind, id)
		if err := unmountS3MetadataPathIfMounted(meta); err != nil {
			umountErr = errors.Join(umountErr, err)
		}
	}

	store, err := requireS3Cow()
	if err != nil {
		return errors.Join(umountErr, err)
	}
	if store != nil {
		if err := store.DeleteByKind(ctx, name, cowKindVolume); err != nil {
			warnVolumeDeleteDeferred(ctx, name, cowKindVolume, err)
		}
		if snap := S3MetadataSnapshotName(id); snap != "" && dropPackageSnapshot {
			if err := store.DeleteByKind(ctx, snap, cowKindSnapshot); err != nil {
				umountErr = errors.Join(umountErr, fmt.Errorf("delete s3 metadata snapshot %s: %w", snap, err))
			}
		}
	}
	delete(s3MetadataMounts, id)
	if state, err := loadS3MetadataState(); err == nil && state != nil {
		if state.Derived != nil {
			delete(state.Derived, id)
		}
		if saveErr := saveS3MetadataState(state); saveErr != nil {
			umountErr = errors.Join(umountErr, saveErr)
		}
	}
	return umountErr
}

func (m *S3Cow) deriveMetadataSnapshotLocked(ctx context.Context, snapshotID, sourceName string) (*cowVolume, error) {
	source := strings.TrimSpace(sourceName)
	if source == "" {
		source = S3MetadataBaseSnapshotName
		if state, err := loadS3MetadataState(); err == nil && state != nil {
			if snap := strings.TrimSpace(state.Base.SnapshotName); snap != "" {
				source = snap
			}
		}
	}
	volumeName := S3MetadataVolumeName(snapshotID)
	if IsS3MetadataBaseName(volumeName) {
		return nil, fmt.Errorf("refusing to derive s3 metadata onto the node-local base")
	}
	devPath, err := m.createOrResolveVolumeFromSnapshot(ctx, source, volumeName)
	if err != nil {
		return nil, fmt.Errorf("derive s3 metadata volume %s from %s: %w", volumeName, source, err)
	}
	return newCowVolume(volumeName, cowKindVolume, 0, devPath), nil
}

// ensureS3MetadataRWVolumeLocked returns the cubecow name that should be
// mounted for package metadata IO. Prefer s3-meta-<id> (volume). When only
// the sealed snap exists, clone it into that volume. Caller must hold
// s3MetadataMu.
func (m *S3Cow) ensureS3MetadataRWVolumeLocked(ctx context.Context, packageID string) (string, error) {
	id := strings.TrimSpace(packageID)
	if id == "" {
		return "", fmt.Errorf("s3 metadata package id is required")
	}
	volName := S3MetadataVolumeName(id)
	if IsS3MetadataBaseName(volName) {
		return "", fmt.Errorf("refusing to use node-local s3 metadata base as package disk")
	}
	volInfo, volErr := m.GetVolumeInfo(ctx, volName)
	volExists, err := cowObjectPresent(volInfo, volErr)
	if err != nil {
		return "", fmt.Errorf("lookup s3 metadata volume %s: %w", volName, err)
	}
	if volExists {
		return volName, nil
	}

	snapName := S3MetadataSnapshotName(id)
	if snapName == "" || IsS3MetadataBaseName(snapName) {
		return "", nil
	}
	snapInfo, snapErr := m.GetVolumeInfo(ctx, snapName)
	snapExists, err := cowObjectPresent(snapInfo, snapErr)
	if err != nil {
		return "", fmt.Errorf("lookup s3 metadata snap %s: %w", snapName, err)
	}
	if !snapExists {
		return "", nil
	}

	// Sealed package snap → private RW volume for mount／sandbox IO.
	if _, err := m.createOrResolveVolumeFromSnapshot(ctx, snapName, volName); err != nil {
		// Fetch import_lvol may already materialize a RW volume under the
		// snap name; use it directly when cloning is not possible.
		if isCowSemantic(err, cubecow.SemInvalidArgument) || isCowSemantic(err, cubecow.SemPreconditionFailed) {
			return snapName, nil
		}
		return "", fmt.Errorf("clone s3 metadata %s from %s: %w", volName, snapName, err)
	}
	return volName, nil
}

func mountS3MetadataLocked(snapshotID, devicePath, mountPath string) error {
	if err := pathutil.ValidateNoTraversal(mountPath); err != nil {
		return fmt.Errorf("invalid s3 metadata mount path: %w", err)
	}
	if prev := s3MetadataMounts[snapshotID]; prev != "" && filepath.Clean(prev) != filepath.Clean(mountPath) {
		if err := unmountS3MetadataPathIfMounted(prev); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("mkdir s3 metadata mount %s: %w", mountPath, err)
	}
	if s3MetadataIsMounted(mountPath) {
		src := s3MetadataMountSource(mountPath)
		want := filepath.Clean(strings.TrimSpace(devicePath))
		if src != "" && (src == want || filepath.Base(src) == filepath.Base(want)) {
			s3MetadataMounts[snapshotID] = filepath.Clean(mountPath)
			return nil
		}
		// Path busy with a different device — never treat as success.
		if err := unmountS3MetadataPathIfMounted(mountPath); err != nil {
			return fmt.Errorf("s3 metadata mount %s busy (%s), umount for %s: %w", mountPath, src, want, err)
		}
	}
	if err := mountS3MetadataDevice(devicePath, mountPath); err != nil {
		return err
	}
	s3MetadataMounts[snapshotID] = filepath.Clean(mountPath)
	return nil
}

func unmountS3MetadataPathIfMounted(mountPath string) error {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		return nil
	}
	if !s3MetadataIsMounted(mountPath) {
		return nil
	}
	if err := unmountS3MetadataPath(mountPath); err != nil {
		return fmt.Errorf("umount s3 metadata %s: %w", mountPath, err)
	}
	for id, p := range s3MetadataMounts {
		if filepath.Clean(p) == filepath.Clean(mountPath) {
			delete(s3MetadataMounts, id)
		}
	}
	return nil
}

func persistDerivedLocked(snapshotID, volName, mountPath string) error {
	state, err := loadS3MetadataState()
	if err != nil {
		return err
	}
	if state.Derived == nil {
		state.Derived = map[string]s3MetadataDerivedRecord{}
	}
	state.Derived[snapshotID] = s3MetadataDerivedRecord{
		SnapshotID: snapshotID,
		VolName:    volName,
		MountPath:  filepath.Clean(mountPath),
	}
	return saveS3MetadataState(state)
}

func dropDerived(snapshotID string) error {
	s3MetadataMu.Lock()
	defer s3MetadataMu.Unlock()
	return dropDerivedLocked(snapshotID)
}

func dropDerivedLocked(snapshotID string) error {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return nil
	}
	state, err := loadS3MetadataState()
	if err != nil {
		return err
	}
	if state.Derived == nil {
		return nil
	}
	delete(state.Derived, id)
	return saveS3MetadataState(state)
}

func deactivateS3Object(ctx context.Context, store cow.Store, name, kind string) error {
	name = strings.TrimSpace(name)
	if store == nil || name == "" {
		return nil
	}
	err := store.DeactivateByKind(ctx, name, kind)
	if err == nil || isCowSemantic(err, cubecow.SemNotFound) {
		return nil
	}
	return err
}

func loadS3MetadataState() (*s3MetadataState, error) {
	state := &s3MetadataState{Derived: map[string]s3MetadataDerivedRecord{}}
	raw, err := readS3MetadataKV()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, state); err != nil {
		return nil, fmt.Errorf("decode s3 metadata state: %w", err)
	}
	if state.Derived == nil {
		state.Derived = map[string]s3MetadataDerivedRecord{}
	}
	return state, nil
}

func saveS3MetadataState(state *s3MetadataState) error {
	if state == nil {
		return nil
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeS3MetadataKV(body)
}

func readS3MetadataKV() ([]byte, error) {
	if testS3MetadataKV != nil {
		return testS3MetadataKV.Get()
	}
	if localStorage == nil || localStorage.db == nil {
		return nil, nil
	}
	b, err := localStorage.db.Get(s3MetadataBucket, s3MetadataStateKey)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

func writeS3MetadataKV(body []byte) error {
	if testS3MetadataKV != nil {
		return testS3MetadataKV.Set(body)
	}
	if localStorage == nil || localStorage.db == nil {
		return nil
	}
	return localStorage.db.Set(s3MetadataBucket, s3MetadataStateKey, body)
}

func formatS3MetadataBaseDeviceImpl(devicePath string) error {
	devicePath = strings.TrimSpace(devicePath)
	if devicePath == "" {
		return errS3EmptyDevicePath(S3MetadataBaseVolumeName, "CreateVolume")
	}
	// Use 4096-byte blocks: s3lvol NVMe devices typically expose 4K
	// physical/logical sectors, and mkfs.ext4 -b 1024 fails with
	// "Invalid argument while setting blocksize; too small for device".
	cmds := [][]string{
		{"mkfs.ext4", "-F", "-O", "^has_journal", "-b", "4096", devicePath},
	}
	for _, cmd := range cmds {
		if _, stderr, err := utils.ExecV(cmd, cmdTimeout); err != nil {
			return fmt.Errorf("mkfs.ext4 s3 metadata base failed:%s", stderr)
		}
	}
	return nil
}

func mountS3MetadataDeviceImpl(devicePath, mountPath string) error {
	devicePath = strings.TrimSpace(devicePath)
	if devicePath == "" {
		return errS3EmptyDevicePath("metadata", "ActivateVolume")
	}
	if _, stderr, err := utils.ExecV([]string{"mount", devicePath, mountPath}, cmdTimeout); err != nil {
		return fmt.Errorf("mount s3 metadata %s at %s failed:%s", devicePath, mountPath, stderr)
	}
	return nil
}

func unmountS3MetadataPathImpl(mountPath string) error {
	if _, stderr, err := utils.ExecV([]string{"umount", mountPath}, cmdTimeout); err != nil {
		if _, _, lazyErr := utils.ExecV([]string{"umount", "-l", mountPath}, cmdTimeout); lazyErr != nil {
			return fmt.Errorf("umount s3 metadata %s failed:%s", mountPath, stderr)
		}
	}
	return nil
}

func s3MetadataIsMountedImpl(mountPath string) bool {
	mountPath = filepath.Clean(strings.TrimSpace(mountPath))
	if mountPath == "" || mountPath == "." {
		return false
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return s3MetadataIsMountedByDev(mountPath)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if filepath.Clean(fields[4]) == mountPath {
			return true
		}
	}
	return s3MetadataIsMountedByDev(mountPath)
}

// s3MetadataMountSource returns the mount source device for mountPath (e.g. /dev/nvme6n1), or "".
func s3MetadataMountSource(mountPath string) string {
	mountPath = filepath.Clean(strings.TrimSpace(mountPath))
	if mountPath == "" || mountPath == "." {
		return ""
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 || filepath.Clean(fields[4]) != mountPath {
			continue
		}
		// mountinfo: ... - fstype source superopts
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep >= 0 && sep+2 < len(fields) {
			return filepath.Clean(fields[sep+2])
		}
	}
	return ""
}

func s3MetadataIsMountedByDev(mountPath string) bool {
	var st, parent unix.Stat_t
	if err := unix.Stat(mountPath, &st); err != nil {
		return false
	}
	if err := unix.Stat(filepath.Dir(mountPath), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

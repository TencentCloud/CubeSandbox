// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow/s3"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// S3Cow is the S3-backed CoW Store. It talks to a dedicated cubecow
// handle started with backend.kind=s3 (same C API as XFS; cubecow
// selects S3LVOL / RCOW from that handle's config). The original
// [XfsCow] implementation is intentionally left untouched.
type S3Cow struct {
	engine cowEngine

	uploadLock   sync.Mutex
	uploadStates map[string]s3UploadEntry // snapshotID → remote uuids / state
}

func newS3CowVolumeManager(engine *cubecow.Engine) *S3Cow {
	return &S3Cow{
		engine:       engine,
		uploadStates: make(map[string]s3UploadEntry),
	}
}

// Name implements [cow.Store].
func (m *S3Cow) Name() string {
	return s3.Name
}

var _ cow.Store = (*S3Cow)(nil)

func (m *S3Cow) CreateDefaultMediumVolume(ctx context.Context, sandboxID, volumeName string, sizeBytes uint64) (*cowVolume, error) {
	name := fmt.Sprintf("sb-%s-%s", sandboxID, volumeName)
	return m.createInitializedVolume(ctx, name, sizeBytes)
}

func (m *S3Cow) createInitializedVolume(ctx context.Context, name string, sizeBytes uint64) (*cowVolume, error) {
	devPath, created, err := m.createOrResolveVolumePath(ctx, name, sizeBytes)
	if err != nil {
		return nil, err
	}
	if created {
		if err := m.initializeNewDefaultMediumVolume(ctx, name, devPath); err != nil {
			return nil, err
		}
	}
	return newCowVolume(name, cowKindVolume, 0, devPath), nil
}

func (m *S3Cow) createOrResolveVolumePath(ctx context.Context, name string, sizeBytes uint64) (string, bool, error) {
	devPath, err := m.engine.CreateVolume(name, sizeBytes)
	if err != nil {
		if !isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", false, err
		}
		devPath, err = m.ResolveDevPath(ctx, name, cowKindVolume)
		if err != nil {
			return "", false, err
		}
		return devPath, false, nil
	}
	devPath, err = requireS3DevicePath(name, "CreateVolume", devPath)
	if err != nil {
		return "", false, err
	}
	return devPath, true, nil
}

func (m *S3Cow) initializeNewDefaultMediumVolume(ctx context.Context, name, devPath string) error {
	devPath, err := requireS3DevicePath(name, "CreateVolume", devPath)
	if err != nil {
		return err
	}
	if err := initDefaultMediumDevice(devPath); err != nil {
		if cleanupErr := m.DeleteByKind(ctx, name, cowKindVolume); cleanupErr != nil {
			return fmt.Errorf("initialize cubecow default medium %s at %s: %w (cleanup failed: %v)", name, devPath, err, cleanupErr)
		}
		return fmt.Errorf("initialize cubecow default medium %s at %s: %w", name, devPath, err)
	}
	return nil
}

func (m *S3Cow) CreateSandboxRootfsFromTemplate(ctx context.Context, sandboxID, templateID string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	sourceName := cowTemplateRootfsName(templateID)
	return m.RollbackDeriveNewGen(ctx, sandboxID, sourceName, gen, desiredSizeBytes)
}

func (m *S3Cow) RollbackDeriveNewGen(ctx context.Context, sandboxID, snapshotRootfsVol string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	if snapshotRootfsVol == "" {
		return nil, fmt.Errorf("snapshot rootfs is required")
	}
	// S3: sandbox rootfs is a RW volume cloned from a RO package snapshot.
	volumeName := fmt.Sprintf("sb-%s-rootfs-gen%d", sandboxID, gen)
	devPath, err := m.createOrResolveVolumeFromSnapshot(ctx, snapshotRootfsVol, volumeName)
	if err != nil {
		return nil, err
	}
	resized, err := m.resizeVolumeIfTooSmall(volumeName, desiredSizeBytes)
	if err != nil {
		return nil, err
	}
	if resized {
		devPath, err = m.ResolveDevPath(ctx, volumeName, cowKindVolume)
		if err != nil {
			return nil, err
		}
	}
	return newCowVolume(volumeName, cowKindVolume, gen, devPath), nil
}

func (m *S3Cow) resizeVolumeIfTooSmall(volumeName string, desiredSizeBytes uint64) (bool, error) {
	if desiredSizeBytes == 0 {
		return false, nil
	}
	info, err := m.engine.GetVolumeInfo(volumeName)
	if err != nil {
		return false, err
	}
	if info == nil || info.SizeBytes >= desiredSizeBytes {
		return false, nil
	}
	if _, _, err := m.engine.ResizeVolume(volumeName, desiredSizeBytes); err != nil {
		return false, err
	}
	return true, nil
}

func (m *S3Cow) CreateTemplateBuildRootfs(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	return m.createInitializedTemplateVolume(ctx, cowTemplateBuildRootfsName(templateID), sizeBytes)
}

func (m *S3Cow) CommitTemplateRootfs(ctx context.Context, sourceName, templateID string) (*cowVolume, error) {
	snapshotName := cowTemplateRootfsName(templateID)
	devPath, err := m.createTemplateSnapshotPath(sourceName, snapshotName)
	if err != nil {
		return nil, err
	}
	return newCowVolume(snapshotName, cowKindSnapshot, 0, devPath), nil
}

func (m *S3Cow) CreateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	name := cowTemplateMemoryName(templateID)
	devPath, err := m.createTemplateVolumePath(name, sizeBytes)
	if err != nil {
		return nil, err
	}
	if err := m.ensureVolumeSizeAtLeast(ctx, name, sizeBytes); err != nil {
		if cleanupErr := m.DeleteByKind(ctx, name, cowKindVolume); cleanupErr != nil {
			return nil, fmt.Errorf("%w (cleanup failed: %v)", err, cleanupErr)
		}
		return nil, err
	}
	resolvedPath, err := m.ResolveDevPath(ctx, name, cowKindVolume)
	if err != nil {
		return nil, err
	}
	if resolvedPath != "" {
		devPath = resolvedPath
	}
	return newCowVolume(name, cowKindVolume, 0, devPath), nil
}

// CommitTemplateMemory derives a writable memory volume for templateID from
// a RO memory snapshot (create_volume_from_snapshot). Source must be a
// snapshot — seal prior package memory before using it as a clone base.
func (m *S3Cow) CommitTemplateMemory(ctx context.Context, sourceName, templateID string, sizeBytes uint64) (*cowVolume, error) {
	volumeName := cowTemplateMemoryName(templateID)
	devPath, err := m.createOrResolveVolumeFromSnapshot(ctx, sourceName, volumeName)
	if err != nil {
		return nil, err
	}
	if sizeBytes > 0 {
		info, infoErr := m.engine.GetVolumeInfo(volumeName)
		if infoErr != nil {
			if cleanupErr := m.DeleteByKind(ctx, volumeName, cowKindVolume); cleanupErr != nil {
				return nil, fmt.Errorf("%w (cleanup failed: %v)", infoErr, cleanupErr)
			}
			return nil, infoErr
		}
		actual := uint64(0)
		if info != nil {
			actual = info.SizeBytes
		}
		if actual < sizeBytes {
			sizeErr := fmt.Errorf("cloned memory volume %s size %d is smaller than requested %d", volumeName, actual, sizeBytes)
			if cleanupErr := m.DeleteByKind(ctx, volumeName, cowKindVolume); cleanupErr != nil {
				return nil, fmt.Errorf("%w (cleanup failed: %v)", sizeErr, cleanupErr)
			}
			return nil, sizeErr
		}
	}
	resolvedPath, err := m.ResolveDevPath(ctx, volumeName, cowKindVolume)
	if err != nil {
		return nil, err
	}
	if resolvedPath != "" {
		devPath = resolvedPath
	}
	return newCowVolume(volumeName, cowKindVolume, 0, devPath), nil
}

func (m *S3Cow) CloneSandboxMemory(ctx context.Context, sandboxID, sourceName string) (*cowVolume, error) {
	return cloneSandboxMemory(ctx, m, sandboxID, sourceName)
}

func (m *S3Cow) createInitializedTemplateVolume(ctx context.Context, name string, sizeBytes uint64) (*cowVolume, error) {
	devPath, err := m.createTemplateVolumePath(name, sizeBytes)
	if err != nil {
		return nil, err
	}
	if err := m.initializeNewDefaultMediumVolume(ctx, name, devPath); err != nil {
		return nil, err
	}
	return newCowVolume(name, cowKindVolume, 0, devPath), nil
}

func (m *S3Cow) createTemplateVolumePath(name string, sizeBytes uint64) (string, error) {
	devPath, err := m.engine.CreateVolume(name, sizeBytes)
	if err != nil {
		if isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", fmt.Errorf("%w: name=%s kind=%s", ErrCowObjectAlreadyExists, name, cowKindVolume)
		}
		return "", err
	}
	return requireS3DevicePath(name, "CreateVolume", devPath)
}

func (m *S3Cow) createTemplateSnapshotPath(sourceName, snapshotName string) (string, error) {
	// Leave inactive: restore resolves device paths via catalog vol name +
	// ResolveDevPath, not a host-local path baked into the package.
	devPath, err := m.engine.CreateSnapshotFromVolume(sourceName, snapshotName, false)
	if err != nil {
		if isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", fmt.Errorf("%w: name=%s kind=%s", ErrCowObjectAlreadyExists, snapshotName, cowKindSnapshot)
		}
		return "", err
	}
	return devPath, nil
}

func (m *S3Cow) ensureVolumeSizeAtLeast(ctx context.Context, name string, requestedSizeBytes uint64) error {
	if requestedSizeBytes == 0 {
		return nil
	}
	actualSizeBytes, err := m.GetSizeBytes(ctx, name)
	if err != nil {
		return err
	}
	if actualSizeBytes >= requestedSizeBytes {
		return nil
	}
	if _, _, err := m.engine.ResizeVolume(name, requestedSizeBytes); err != nil {
		return fmt.Errorf("resize cubecow volume %s from %d to %d bytes: %w", name, actualSizeBytes, requestedSizeBytes, err)
	}
	actualSizeBytes, err = m.GetSizeBytes(ctx, name)
	if err != nil {
		return err
	}
	if actualSizeBytes < requestedSizeBytes {
		return fmt.Errorf("cubecow volume %s size %d is smaller than requested %d", name, actualSizeBytes, requestedSizeBytes)
	}
	return nil
}

func (m *S3Cow) createOrResolveVolumeFromSnapshot(ctx context.Context, sourceSnapshot, volumeName string) (string, error) {
	devPath, err := m.engine.CreateVolumeFromSnapshot(sourceSnapshot, volumeName)
	if err != nil {
		if !isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", err
		}
		devPath, err = m.ResolveDevPath(ctx, volumeName, cowKindVolume)
		if err != nil {
			return "", err
		}
		return devPath, nil
	}
	return requireS3DevicePath(volumeName, "CreateVolumeFromSnapshot", devPath)
}

func (m *S3Cow) createOrResolveSnapshotPathFromSource(ctx context.Context, sourceName, snapshotName string) (string, error) {
	devPath, err := m.engine.CreateSnapshotFromVolume(sourceName, snapshotName, true)
	if err != nil {
		if !isCowSemantic(err, cubecow.SemAlreadyExists) {
			return "", err
		}
		if err := m.ensureSnapshotOrigin(sourceName, snapshotName); err != nil {
			return "", err
		}
		devPath, err = m.ResolveDevPath(ctx, snapshotName, cowKindSnapshot)
		if err != nil {
			return "", err
		}
		return devPath, nil
	}
	return requireS3DevicePath(snapshotName, "CreateSnapshotFromVolume", devPath)
}

func (m *S3Cow) ensureSnapshotOrigin(sourceName, snapshotName string) error {
	result, err := m.engine.ListSnapshots(sourceName, 0, "")
	if err != nil {
		return fmt.Errorf("verify existing snapshot %s origin from %s: %w", snapshotName, sourceName, err)
	}
	if result == nil {
		return fmt.Errorf("%w: name=%s kind=%s origin=%s", ErrCowObjectAlreadyExists, snapshotName, cowKindSnapshot, sourceName)
	}
	for _, snapshot := range result.Snapshots {
		if snapshot.Name != snapshotName {
			continue
		}
		if snapshot.OriginVolume != "" && snapshot.OriginVolume != sourceName {
			return fmt.Errorf("existing snapshot %s origin %s does not match expected %s", snapshotName, snapshot.OriginVolume, sourceName)
		}
		return nil
	}
	return fmt.Errorf("%w: name=%s kind=%s origin=%s", ErrCowObjectAlreadyExists, snapshotName, cowKindSnapshot, sourceName)
}

// DeleteByKind removes a cubecow object, treating a snapshot that s3lvol
// refuses as busy as removed. See [isS3DeleteBusy].
func (m *S3Cow) DeleteByKind(ctx context.Context, name, kind string) error {
	err := m.deleteByKind(ctx, name, kind)
	if err == nil || !isS3DeleteBusy(err) {
		return err
	}
	normalized, kindErr := normalizeCowKind(kind)
	if kindErr != nil || normalized != cowKindSnapshot {
		// Volumes follow the sandbox lifecycle and nothing else should be
		// holding one open. Busy there is a bug worth surfacing, not an
		// artifact of an export we cannot release.
		return err
	}
	CubeLog.Warnf("s3 delete snapshot %s refused as busy; recording it as deleted and leaking the object: %v",
		name, err)
	return nil
}

// isS3DeleteBusy matches s3lvol's refusal to delete an object it still
// considers referenced ("precondition failed: s3lvol rpc: Device or resource
// busy").
//
// Nothing on this node can clear that reference: it lives inside s3lvol —
// typically an export it has not released, and cubecow exposes no
// release_export — while host activation has already been torn down. Retrying
// returns the same answer forever, so a caller that honours the error never
// finishes: the package is swept half way, its directory and catalog stay
// behind, and the template or sandbox it belongs to can never be deleted.
//
// Leaking the object costs S3 space. The warning above is the only record
// that it happened, so it names the object.
//
// Scope is deliberately this one error from this one call. Busy from the
// export path (s3lvol draining an lvstore) means the export never happened
// and must keep failing.
func isS3DeleteBusy(err error) bool {
	if !isCowSemantic(err, cubecow.SemPreconditionFailed) {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "busy")
}

func (m *S3Cow) deleteByKind(ctx context.Context, name, kind string) error {
	_ = ctx
	deleteFn, err := m.deleteFunc(kind)
	if err != nil {
		return err
	}
	if err = deleteFn(name); err == nil || isCowSemantic(err, cubecow.SemNotFound) {
		return nil
	}
	// FIX-3: the recorded kind may not match the object's real cubecow type
	// (e.g. an incremental memory snapshot recorded/derived as a volume).
	// cubecow returns SemInvalidArgument ("is a snapshot; use delete_snapshot")
	// in that case. Retry with the opposite delete function so cleanup stays
	// kind-agnostic and idempotent rather than leaking the object.
	if isCowSemantic(err, cubecow.SemInvalidArgument) {
		if otherFn := m.oppositeDeleteFunc(kind); otherFn != nil {
			if retryErr := otherFn(name); retryErr == nil || isCowSemantic(retryErr, cubecow.SemNotFound) {
				return nil
			}
		}
		return err
	}
	if isCowSemantic(err, cubecow.SemIoError) {
		if retryErr := deleteFn(name); retryErr == nil || isCowSemantic(retryErr, cubecow.SemNotFound) {
			return nil
		} else {
			return retryErr
		}
	}
	return err
}

func (m *S3Cow) DeactivateByKind(ctx context.Context, name, kind string) error {
	_ = ctx
	if _, err := m.deleteFunc(kind); err != nil {
		return err
	}
	return m.engine.DeactivateVolume(name)
}

func (m *S3Cow) ResolveDevPath(ctx context.Context, name, kind string) (string, error) {
	_ = ctx
	if _, err := m.deleteFunc(kind); err != nil {
		return "", err
	}
	// Always ActivateVolume. s3lvol may reassign /dev/nvmeXn1 after
	// deactivate／reactivate; GetVolumeInfo.DevicePath is cubecow's last
	// cached path and must not be handed to cube-runtime as --memory-vol.
	devPath, err := m.engine.ActivateVolume(name)
	if err != nil {
		return "", err
	}
	return requireS3DevicePath(name, "ActivateVolume", devPath)
}

// errS3EmptyDevicePath explains that s3lvol activated the object but the host
// NVMe-oF path is missing. Callers must not continue to mkfs/mount/format.
func errS3EmptyDevicePath(name, op string) error {
	name = strings.TrimSpace(name)
	op = strings.TrimSpace(op)
	if op == "" {
		op = "activate"
	}
	return fmt.Errorf("s3 cubecow object %q has empty device_path after %s (s3lvol/NVMe-oF host path not ready; refuse mkfs/mount)", name, op)
}

func requireS3DevicePath(name, op, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errS3EmptyDevicePath(name, op)
	}
	return path, nil
}

func (m *S3Cow) GetSizeBytes(ctx context.Context, name string) (uint64, error) {
	_ = ctx
	info, err := m.engine.GetVolumeInfo(name)
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, fmt.Errorf("cubecow object %q has empty info", name)
	}
	return info.SizeBytes, nil
}

func (m *S3Cow) GetVolumeInfo(ctx context.Context, name string) (*cubecow.Volume, error) {
	_ = ctx
	return m.engine.GetVolumeInfo(name)
}

func (m *S3Cow) GetMetrics(ctx context.Context) (map[string]uint64, error) {
	_ = ctx
	return m.engine.GetMetrics()
}

func (m *S3Cow) deleteFunc(kind string) (func(string) error, error) {
	switch kind {
	case cowKindVolume:
		return m.engine.DeleteVolume, nil
	case cowKindSnapshot:
		return m.engine.DeleteSnapshot, nil
	default:
		return nil, fmt.Errorf("unsupported cubecow kind %q", kind)
	}
}

// oppositeDeleteFunc returns the delete function for the other cubecow kind, so
// DeleteByKind can recover when the recorded kind does not match the object's
// real type. Returns nil for an unrecognized kind.
func (m *S3Cow) oppositeDeleteFunc(kind string) func(string) error {
	switch kind {
	case cowKindVolume:
		return m.engine.DeleteSnapshot
	case cowKindSnapshot:
		return m.engine.DeleteVolume
	default:
		return nil
	}
}

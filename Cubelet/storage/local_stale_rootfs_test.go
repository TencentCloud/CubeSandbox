// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const testRootfsVolume = "cube_rootfs_rw"

func pauseResumeRootfsContext() *workflow.CreateContext {
	opts := pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause",
	})
	opts.ReqInfo.Containers = []*cubebox.ContainerConfig{{
		VolumeMounts: []*cubebox.VolumeMounts{{
			Name:          testRootfsVolume,
			ContainerPath: "/",
		}},
	}}
	return opts
}

// staleRootfsAwareManager models the backend's observed behavior: deriving a
// sandbox rootfs whose name already exists fails with ErrCowObjectAlreadyExists
// until the stale object is deleted.
type staleRootfsAwareManager struct {
	*fakeCowVolumeManager
	deleted     bool
	createCalls int
}

func (m *staleRootfsAwareManager) DeleteByKind(ctx context.Context, name, kind string) error {
	if err := m.fakeCowVolumeManager.DeleteByKind(ctx, name, kind); err != nil {
		return err
	}
	if name == "sb-sb-1-rootfs-gen0" {
		m.deleted = true
	}
	return nil
}

func (m *staleRootfsAwareManager) CreateSandboxRootfsFromTemplate(ctx context.Context, sandboxID, templateID string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	m.createCalls++
	if !m.deleted {
		return nil, ErrCowObjectAlreadyExists
	}
	return m.fakeCowVolumeManager.CreateSandboxRootfsFromTemplate(ctx, sandboxID, templateID, gen, desiredSizeBytes)
}

func allocateRootfs(t *testing.T, s *local, opts *workflow.CreateContext, backend string) (*cowVolume, error) {
	t.Helper()
	opts.ReqInfo.Backend = backend
	return s.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "snap-pause", testRootfsVolume, 0)
}

// The regression this fix exists for: a failed resume left gen0 behind and the
// next resume used to fail forever. The first derive collides, the permission
// authorizes one delete, and the second derive succeeds.
func TestAllocateSnapshotRootfsRetryRecoversFromStaleDerive(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	l := &local{cowManager: manager}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true

	volume, err := allocateRootfs(t, l, opts, cow.BackendXFS)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, "sb-sb-1-rootfs-gen0", volume.VolumeName)
	assert.Equal(t, []fakeCowDeleteCall{{
		name: "sb-sb-1-rootfs-gen0",
		kind: cowKindSnapshot,
	}}, manager.deleteCalls)
	assert.Equal(t, 2, manager.createCalls, "one colliding derive, one clean derive")
}

// A healthy resume must not delete anything: derive-first means the reclaim is
// only reached when the backend reports a real collision.
func TestAllocateSnapshotRootfsHealthyResumeDoesNotDelete(t *testing.T) {
	t.Parallel()

	fake := &fakeCowVolumeManager{}
	l := &local{cowManager: fake}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true

	_, err := allocateRootfs(t, l, opts, cow.BackendXFS)
	require.NoError(t, err)
	assert.Empty(t, fake.deleteCalls)
	assert.Len(t, fake.createSnapshotCalls, 1)
}

// The permission is required; without it the collision stays an error and the
// backend object is left alone.
func TestAllocateSnapshotRootfsKeepsCollisionWithoutReclaimPermission(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	l := &local{cowManager: manager}
	opts := pauseResumeRootfsContext()

	_, err := allocateRootfs(t, l, opts, cow.BackendXFS)
	require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	assert.Empty(t, manager.deleteCalls)
	assert.Equal(t, 1, manager.createCalls)
}

// A second Default emptyDir volume shares the derive name; reclaiming on its
// collision would delete the rootfs the first allocation just created.
func TestAllocateSnapshotRootfsDoesNotReclaimNonRootfsVolume(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	l := &local{cowManager: manager}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true

	_, err := l.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "snap-pause", "scratch", 0)
	require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	assert.Empty(t, manager.deleteCalls)
}

// S3 reuses its writable volume idempotently; #1847 is a reflink/JFS bug.
// Keep S3 out of the destructive path until it has its own verification.
func TestAllocateSnapshotRootfsDoesNotReclaimS3(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	l := &local{s3CowManager: manager}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true

	_, err := allocateRootfs(t, l, opts, cow.BackendS3)
	require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	assert.Empty(t, manager.deleteCalls)
}

// If the stale object cannot be removed, deriving would hit the same
// already-exists error. Fail with the cleanup cause instead of hiding it.
func TestAllocateSnapshotRootfsStopsWhenStaleRootfsCannotBeRemoved(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{
		deleteErrs: map[string]error{
			"sb-sb-1-rootfs-gen0|" + cowKindSnapshot: errors.New("object busy"),
		},
	}}
	l := &local{cowManager: manager}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true

	_, err := allocateRootfs(t, l, opts, cow.BackendXFS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discard stale sandbox rootfs sb-sb-1-rootfs-gen0")
	assert.Contains(t, err.Error(), "object busy")
	assert.Equal(t, 1, manager.createCalls)
}

// The storage id and the Master-supplied desired id must agree before a
// destructive reclaim uses the former.
func TestAllocateSnapshotRootfsRejectsMismatchedDesiredID(t *testing.T) {
	t.Parallel()

	fake := &fakeCowVolumeManager{}
	l := &local{cowManager: fake}
	opts := pauseResumeRootfsContext()
	opts.ReclaimStaleRootfs = true
	opts.ReqInfo.Annotations[constants.MasterAnnotationDesiredSandboxID] = "sb-other"

	_, err := allocateRootfs(t, l, opts, cow.BackendXFS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `sandbox id "sb-1" does not match desired id "sb-other"`)
	assert.Empty(t, fake.createSnapshotCalls)
}

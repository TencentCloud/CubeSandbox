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
)

func pauseResumeAnnotatedContext() *workflow.CreateContext {
	return pauseResumeCreateContext("sb-1", map[string]string{
		constants.MasterAnnotationPauseSnapshotID:   "snap-pause",
		constants.MasterAnnotationRuntimeSnapshotID: "snap-pause",
	})
}

// A pause resume that passed the paused-replace gate owns the stale
// sb-<id>-rootfs-gen0 name: the retry must discard the interrupted derive
// instead of colliding with it through the backend's snapshot-origin check.
func TestAllocateSnapshotRootfsReclaimsStalePauseResumeRootfs(t *testing.T) {
	t.Parallel()

	fake := &fakeCowVolumeManager{}
	l := &local{cowManager: fake}
	opts := pauseResumeAnnotatedContext()
	opts.ReclaimStaleRootfs = true

	volume, err := l.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "snap-pause", 0)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, "sb-sb-1-rootfs-gen0", volume.VolumeName)
	assert.Equal(t, []fakeCowDeleteCall{{
		name: "sb-sb-1-rootfs-gen0",
		kind: cowKindSnapshot,
	}}, fake.deleteCalls)
	require.Len(t, fake.createSnapshotCalls, 1)
}

// staleRootfsAwareManager models the backend's observed behavior: deriving a
// sandbox rootfs whose name already exists fails with ErrCowObjectAlreadyExists
// until the stale object is deleted.
type staleRootfsAwareManager struct {
	*fakeCowVolumeManager
	deleted bool
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
	if !m.deleted {
		return nil, ErrCowObjectAlreadyExists
	}
	return m.fakeCowVolumeManager.CreateSandboxRootfsFromTemplate(ctx, sandboxID, templateID, gen, desiredSizeBytes)
}

// The regression this fix exists for: a failed resume left gen0 behind and
// the next resume used to fail forever. With the gate's permission the retry
// removes the stale object and derives successfully.
func TestAllocateSnapshotRootfsRetryRecoversFromStaleDerive(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	opts := pauseResumeAnnotatedContext()
	opts.ReclaimStaleRootfs = true

	volume, err := (&local{cowManager: manager}).allocateSnapshotRootfs(
		context.Background(), opts, "sb-1", "snap-pause", 0)
	require.NoError(t, err)
	require.NotNil(t, volume)
}

// Without the gate's proof the retry must stay on the backend's collision
// error rather than deleting a rootfs that may still have an owner.
func TestAllocateSnapshotRootfsKeepsCollisionWithoutReclaimPermission(t *testing.T) {
	t.Parallel()

	manager := &staleRootfsAwareManager{fakeCowVolumeManager: &fakeCowVolumeManager{}}
	opts := pauseResumeAnnotatedContext()

	_, err := (&local{cowManager: manager}).allocateSnapshotRootfs(
		context.Background(), opts, "sb-1", "snap-pause", 0)
	require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
}

// The flag is permission, not a command: an ordinary create (no pause
// annotation) or a resume whose gate did not prove the old runtime gone must
// not delete a rootfs that may belong to a live sandbox.
func TestAllocateSnapshotRootfsReclaimsOnlyWithPauseResumePermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		annot   map[string]string
		reclaim bool
	}{
		{
			name:    "pause resume without gate permission",
			annot:   pauseResumeAnnotatedContext().ReqInfo.Annotations,
			reclaim: false,
		},
		{
			name:    "ordinary create with gate permission",
			annot:   map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1"},
			reclaim: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCowVolumeManager{}
			l := &local{cowManager: fake}
			opts := pauseResumeCreateContext("sb-1", tt.annot)
			opts.ReclaimStaleRootfs = tt.reclaim

			_, err := l.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "tpl-1", 0)
			require.NoError(t, err)
			assert.Empty(t, fake.deleteCalls)
			require.Len(t, fake.createSnapshotCalls, 1)
		})
	}
}

// S3 mints the sandbox rootfs as a writable volume, so reclaim must use that
// kind; the reflink snapshot kind would hit NotFound and leave the object.
func TestAllocateSnapshotRootfsReclaimsStaleS3RootfsAsVolume(t *testing.T) {
	t.Parallel()

	fake := &fakeCowVolumeManager{}
	l := &local{s3CowManager: fake}
	opts := pauseResumeAnnotatedContext()
	opts.ReqInfo.Backend = cow.BackendS3
	opts.ReclaimStaleRootfs = true

	_, err := l.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "snap-pause", 0)
	require.NoError(t, err)
	assert.Equal(t, []fakeCowDeleteCall{{
		name: "sb-sb-1-rootfs-gen0",
		kind: cowKindVolume,
	}}, fake.deleteCalls)
}

// If the stale object cannot be removed, deriving would hit the same
// already-exists error. Fail with the cleanup cause instead of hiding it.
func TestAllocateSnapshotRootfsStopsWhenStaleRootfsCannotBeRemoved(t *testing.T) {
	t.Parallel()

	fake := &fakeCowVolumeManager{
		deleteErrs: map[string]error{
			"sb-sb-1-rootfs-gen0|" + cowKindSnapshot: errors.New("object busy"),
		},
	}
	l := &local{cowManager: fake}
	opts := pauseResumeAnnotatedContext()
	opts.ReclaimStaleRootfs = true

	_, err := l.allocateSnapshotRootfs(context.Background(), opts, "sb-1", "snap-pause", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discard stale sandbox rootfs sb-sb-1-rootfs-gen0")
	assert.Contains(t, err.Error(), "object busy")
	assert.Empty(t, fake.createSnapshotCalls)
}

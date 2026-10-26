// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestRollbackDisksFromSnapshotSpecReplacesCurrentRootfs(t *testing.T) {
	spec := &CubeboxSnapshotSpec{
		Disk: json.RawMessage(`[
			{"path":"/dev/mapper/old-root","rate_limiter_config":{"bandwidth":{"size":1}}},
			{"path":"/dev/mapper/data"}
		]`),
	}

	disks, err := rollbackDisksFromSnapshotSpec(spec, "", "/dev/mapper/old-root", "/dev/mapper/new-root")
	require.NoError(t, err)
	require.Len(t, disks, 2)
	assert.Equal(t, "/dev/mapper/new-root", disks[0].Path)
	assert.Equal(t, "disk-0", disks[0].ID)
	assert.Equal(t, "/dev/mapper/data", disks[1].Path)
	assert.NotEmpty(t, disks[0].RateLimiterConfig)
}

func TestRollbackDisksFromSnapshotSpecRejectsMissingRootfs(t *testing.T) {
	spec := &CubeboxSnapshotSpec{
		Disk: json.RawMessage(`[{"path":"/dev/mapper/data"}]`),
	}

	disks, err := rollbackDisksFromSnapshotSpec(spec, "", "/dev/mapper/old-root", "/dev/mapper/new-root")
	require.Error(t, err)
	assert.Nil(t, disks)
}

func TestRollbackDisksFromSnapshotSpecMatchesRootfsMountName(t *testing.T) {
	spec := &CubeboxSnapshotSpec{
		Disk: json.RawMessage(`[
			{"path":"/dev/mapper/stale-root","volume_source":"root"},
			{"path":"/dev/mapper/data","volume_source":"data"}
		]`),
	}

	disks, err := rollbackDisksFromSnapshotSpec(spec, "root", "/dev/mapper/current-root", "/dev/mapper/new-root")
	require.NoError(t, err)
	require.Len(t, disks, 2)
	assert.Equal(t, "/dev/mapper/new-root", disks[0].Path)
	assert.Equal(t, "/dev/mapper/data", disks[1].Path)
}

func TestSnapshotStateDirUsesSnapshotSubdir(t *testing.T) {
	assert.Equal(t, "/snapshots/s1/snapshot", snapshotStateDir("/snapshots/s1"))
	assert.Equal(t, "/snapshots/s1/snapshot", snapshotStateDir("/snapshots/s1/snapshot"))
	assert.Equal(t, "file:///snapshots/s1", snapshotStateDir("file:///snapshots/s1"))
}

func TestDeactivateRollbackPackageObjectsHandlesNil(t *testing.T) {
	require.NotPanics(t, func() {
		deactivateRollbackPackageObjects(context.Background(), "s3", nil, false)
	})
}

func TestDeactivateRollbackPackageObjectsKeepsMemoryAfterRestore(t *testing.T) {
	refs := &storage.CowRollbackSnapshotRefs{
		Rootfs: &storage.CowSnapshotObject{Name: "tpl-snap-1-rootfs", Kind: "snapshot"},
		Memory: &storage.CowSnapshotObject{Name: "tpl-snap-1-memory-snap", Kind: "snapshot"},
	}

	for _, tc := range []struct {
		name     string
		backend  string
		restored bool
		want     []string
	}{
		{name: "s3 restore succeeded keeps memory attached", backend: "s3", restored: true, want: []string{"tpl-snap-1-rootfs"}},
		{name: "s3 restore failed releases both", backend: "s3", restored: false, want: []string{"tpl-snap-1-memory-snap", "tpl-snap-1-rootfs"}},
		{name: "xfs releases both as before", backend: "xfs", restored: true, want: []string{"tpl-snap-1-memory-snap", "tpl-snap-1-rootfs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			original := deactivateRollbackObject
			deactivateRollbackObject = func(_ context.Context, _, name, _ string) error {
				got = append(got, name)
				return nil
			}
			defer func() { deactivateRollbackObject = original }()

			deactivateRollbackPackageObjects(context.Background(), tc.backend, refs, tc.restored)
			assert.Equal(t, tc.want, got)
		})
	}
}

func newCubeboxWithStatusForTest(id string, status cubeboxstore.Status) *cubeboxstore.CubeBox {
	statusStorage := cubeboxstore.StoreStatus(status)
	container := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: id},
		Status:   statusStorage,
	}
	return &cubeboxstore.CubeBox{
		Metadata:           cubeboxstore.Metadata{ID: id, CreatedAt: time.Now().UnixNano()},
		FirstContainerName: id,
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{id: container},
		},
	}
}

func TestSetSandboxRollingBackTogglesEveryContainerStatus(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-rb", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	require.False(t, cb.GetStatus().Get().RollingBack)

	setSandboxRollingBack(cb, true)
	assert.True(t, cb.GetStatus().Get().RollingBack, "flag must be set on FirstContainer status")

	setSandboxRollingBack(cb, false)
	assert.False(t, cb.GetStatus().Get().RollingBack, "flag must be cleared on FirstContainer status")
}

func TestSetSandboxRollingBackHandlesNilCubebox(t *testing.T) {
	require.NotPanics(t, func() { setSandboxRollingBack(nil, true) })
	require.NotPanics(t, func() { setSandboxRollingBack(nil, false) })
}

func TestRunRollbackWithPreparedGuestMetricsOrdersPreparationBeforeRestore(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-order", cubeboxstore.Status{StartedAt: 1})
	steps := make([]string, 0, 2)

	err := runRollbackWithPreparedGuestMetrics(
		cb,
		func() error {
			steps = append(steps, "preflight")
			return nil
		},
		func() error {
			require.True(t, cb.GetStatus().Get().RollingBack)
			steps = append(steps, "prepare")
			return nil
		},
		func() error {
			require.True(t, cb.GetStatus().Get().RollingBack)
			steps = append(steps, "restore")
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, []string{"preflight", "prepare", "restore"}, steps)
	require.False(t, cb.GetStatus().Get().RollingBack)
}

func TestRunRollbackWithPreparedGuestMetricsStopsBeforeRestoreWhenPreparationFails(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-prepare-fail", cubeboxstore.Status{StartedAt: 1})
	restoreCalled := false

	err := runRollbackWithPreparedGuestMetrics(
		cb,
		func() error { return nil },
		func() error { return errors.New("metadata unavailable") },
		func() error {
			restoreCalled = true
			return nil
		},
	)

	require.ErrorContains(t, err, "prepare guest metrics epoch")
	require.False(t, restoreCalled)
	require.False(t, cb.GetStatus().Get().RollingBack)
}

func TestRunRollbackWithPreparedGuestMetricsKeepsCurrentEpochWhenPreflightFails(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-preflight-fail", cubeboxstore.Status{StartedAt: 1})
	startedAt := time.Date(2026, time.July, 21, 1, 0, 0, 0, time.UTC)
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, startedAt))
	previous := cb.GuestMetricsEpochCopy()
	prepareCalled := false
	restoreCalled := false

	err := runRollbackWithPreparedGuestMetrics(
		cb,
		func() error { return errors.New("task is unavailable") },
		func() error {
			prepareCalled = true
			return cb.PrepareRollbackGuestMetricsEpoch(startedAt.Add(time.Minute))
		},
		func() error {
			restoreCalled = true
			return nil
		},
	)

	require.ErrorContains(t, err, "preflight sandbox runtime rollback")
	require.False(t, prepareCalled)
	require.False(t, restoreCalled)
	require.Equal(t, previous, cb.GuestMetricsEpochCopy())
	require.False(t, cb.GetStatus().Get().RollingBack)
}

func TestRunRollbackWithPreparedGuestMetricsKeepsPreparedEpochOnRestoreFailure(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-restore-fail", cubeboxstore.Status{StartedAt: 1})
	startedAt := time.Date(2026, time.July, 21, 1, 0, 0, 0, time.UTC)

	err := runRollbackWithPreparedGuestMetrics(
		cb,
		func() error { return nil },
		func() error { return cb.PrepareRollbackGuestMetricsEpoch(startedAt) },
		func() error { return errors.New("shim restore failed after runtime mutation") },
	)

	require.ErrorContains(t, err, "restore sandbox runtime")
	require.Equal(t, cubeboxstore.GuestMetricsEpochPrepared, cb.GuestMetricsEpochCopy().State)
	require.False(t, cb.GetStatus().Get().RollingBack)
}

func TestResetSandboxStatusAfterRollbackScrubsTerminatedMarkers(t *testing.T) {
	preStarted := time.Now().Add(-time.Hour).UnixNano()
	pre := cubeboxstore.Status{
		StartedAt:  preStarted,
		Unknown:    true,
		FinishedAt: time.Now().UnixNano(),
		ExitCode:   137,
		Reason:     "oom-spurious",
		Message:    "leftover from concurrent TaskExit",
		PausedAt:   time.Now().UnixNano(),
		PausingAt:  time.Now().UnixNano(),
	}
	cb := newCubeboxWithStatusForTest("sb-reset", pre)

	resetSandboxStatusAfterRollback(cb)

	got := cb.GetStatus().Get()
	assert.False(t, got.Unknown, "Unknown must be cleared so IsTerminated() returns false")
	assert.Equal(t, int64(0), got.FinishedAt, "FinishedAt must be cleared")
	assert.Equal(t, int32(0), got.ExitCode, "ExitCode must be cleared")
	assert.Empty(t, got.Reason, "Reason must be cleared")
	assert.Empty(t, got.Message, "Message must be cleared")
	assert.Equal(t, int64(0), got.PausedAt, "PausedAt must be cleared")
	assert.Equal(t, int64(0), got.PausingAt, "PausingAt must be cleared")
	assert.Equal(t, preStarted, got.StartedAt, "StartedAt must be preserved when non-zero")
}

func TestResetSandboxStatusAfterRollbackBootstrapsStartedAtWhenZero(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-reset-zero", cubeboxstore.Status{
		Unknown:    true,
		FinishedAt: time.Now().UnixNano(),
	})

	before := time.Now().UnixNano()
	resetSandboxStatusAfterRollback(cb)
	after := time.Now().UnixNano()

	got := cb.GetStatus().Get()
	assert.GreaterOrEqual(t, got.StartedAt, before, "StartedAt must be bootstrapped")
	assert.LessOrEqual(t, got.StartedAt, after, "StartedAt must be bootstrapped to ~now")
}

func TestResetSandboxStatusAfterRollbackHandlesNilCubebox(t *testing.T) {
	require.NotPanics(t, func() { resetSandboxStatusAfterRollback(nil) })
}

// TestHandleContainerExitSkipsRollingBack verifies the events.go fast-path:
// with RollingBack=true, handleContainerExit must short-circuit BEFORE
// touching cntr.Container.Task() / status mutation so that the OLD VM's
// TaskExit cannot stamp FinishedAt onto the cubebox during a rollback.
func TestHandleContainerExitSkipsRollingBack(t *testing.T) {
	pre := cubeboxstore.Status{
		StartedAt:   time.Now().Add(-time.Minute).UnixNano(),
		Pid:         12345,
		RollingBack: true,
	}
	cntr := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "ctr-rb"},
		Status:   cubeboxstore.StoreStatus(pre),
	}

	em := (*eventMonitor)(nil)
	exit := &eventtypes.TaskExit{
		ContainerID: "ctr-rb",
		ID:          "ctr-rb",
		Pid:         12345,
		ExitStatus:  4294967295,
	}

	err := em.handleContainerExit(context.Background(), exit, cntr)
	require.NoError(t, err, "handler must short-circuit cleanly when RollingBack is set")

	got := cntr.Status.Get()
	assert.Equal(t, int64(0), got.FinishedAt, "FinishedAt must remain unstamped")
	assert.Equal(t, int32(0), got.ExitCode, "ExitCode must remain unstamped")
	assert.Equal(t, uint32(12345), got.Pid, "Pid must remain unchanged")
	assert.True(t, got.RollingBack, "RollingBack flag must survive the handler")
}

func TestHandleContainerExitSkipsPauseLifecycle(t *testing.T) {
	now := time.Now().UnixNano()
	cases := []struct {
		name string
		pre  cubeboxstore.Status
	}{
		{
			name: "pausing",
			pre: cubeboxstore.Status{
				StartedAt: now - int64(time.Minute),
				PausingAt: now,
				Pid:       99,
			},
		},
		{
			name: "paused",
			pre: cubeboxstore.Status{
				StartedAt: now - int64(time.Minute),
				PausedAt:  now,
				Pid:       99,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cntr := &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{ID: "ctr-pause"},
				Status:   cubeboxstore.StoreStatus(tc.pre),
			}
			em := (*eventMonitor)(nil)
			err := em.handleContainerExit(context.Background(), &eventtypes.TaskExit{
				ContainerID: "ctr-pause",
				ID:          "ctr-pause",
				Pid:         99,
				ExitStatus:  0,
			}, cntr)
			require.NoError(t, err)
			got := cntr.Status.Get()
			assert.Equal(t, int64(0), got.FinishedAt)
			assert.Equal(t, tc.pre.PausingAt, got.PausingAt)
			assert.Equal(t, tc.pre.PausedAt, got.PausedAt)
			assert.True(t, cntr.Status.IsPaused())
		})
	}
}

func TestScanDeadContainerSkipsRollingBack(t *testing.T) {
	staleFinishedAt := time.Now().Add(-time.Hour).UnixNano()
	cb := newCubeboxWithStatusForTest("sb-deadgc-skip", cubeboxstore.Status{
		StartedAt:   time.Now().UnixNano(),
		Unknown:     true,
		FinishedAt:  staleFinishedAt,
		RollingBack: true,
	})

	scanDeadContainer(context.Background(), []*cubeboxstore.CubeBox{cb}, nil, time.Hour)

	got := cb.GetStatus().Get()
	assert.True(t, got.RollingBack, "RollingBack flag must survive the scan")
	assert.Equal(t, staleFinishedAt, got.FinishedAt, "scanDeadContainer must not touch a rolling-back cubebox")
	assert.True(t, got.Unknown, "Unknown must be left as-is for the rollback path to fix")
}

// TestScanDeadContainerSkipsCreatingSandbox reproduces the create/DeadGC race:
// while a sandbox sits in the CONTAINER_CREATED window (store entry saved before
// runContainer binds the containerd task), DeadGC must NOT call RecoverContainer,
// which would find no task and wrongly stamp Unknown=true -- making a follow-up
// pause fail with "sandbox is not running". A nil containerd client guarantees
// the test panics/errors if the scan ever reaches RecoverContainer for this cb.
func TestScanDeadContainerSkipsCreatingSandbox(t *testing.T) {
	cb := newCubeboxWithStatusForTest("sb-deadgc-creating", cubeboxstore.Status{
		CreatedAt: time.Now().UnixNano(),
	})

	// The nil client is the assertion: RecoverContainer -> client.LoadContainer
	// dereferences the nil *containerd.Client and panics. NotPanics therefore
	// proves the create-window guard short-circuited before reaching it.
	assert.NotPanics(t, func() {
		scanDeadContainer(context.Background(), []*cubeboxstore.CubeBox{cb}, nil, time.Hour)
	}, "DeadGC must skip a creating sandbox before touching the containerd client")

	assert.False(t, cb.GetStatus().IsTerminated(), "a creating sandbox must not read as terminated")
	got := cb.GetStatus().Get()
	assert.False(t, got.Unknown, "a creating sandbox must not be stamped Unknown by DeadGC")
	assert.Equal(t, int64(0), got.FinishedAt, "a creating sandbox must not be stamped FinishedAt by DeadGC")
}

// TestScanDeadContainerCreateSkipIsBounded verifies the create skip is bounded:
// once CreatedAt is older than createStuckThreshold the task was never bound and
// the entry is genuinely stuck, so DeadGC must NOT short-circuit and instead
// falls through to probe it. We drive scanDeadContainer with a nil client so
// reaching RecoverContainer -> client.LoadContainer panics; assert.Panics thus
// proves the guard let the stuck entry through rather than re-stating the guard
// predicate literally.
func TestScanDeadContainerCreateSkipIsBounded(t *testing.T) {
	stuck := newCubeboxWithStatusForTest("sb-deadgc-stuck-create", cubeboxstore.Status{
		CreatedAt: time.Now().Add(-2 * createStuckThreshold).UnixNano(),
	})

	assert.Panics(t, func() {
		scanDeadContainer(context.Background(), []*cubeboxstore.CubeBox{stuck}, nil, time.Hour)
	}, "a long-stuck creating sandbox must fall out of the skip window and be probed")
}

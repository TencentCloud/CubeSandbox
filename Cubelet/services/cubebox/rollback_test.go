// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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

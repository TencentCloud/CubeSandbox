// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"

	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
)

func pendingRestoreFixtures(id string) (*cubeboxstore.CubeBox, *cubeboxstore.CubeBox, *cubeboxstore.CubeBox) {
	tomb := sandboxWithResourceForTest(id, cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = id
	snapshot := tomb.DeepCopy()
	debris := newCubeboxWithStatusForTest(id, cubeboxstore.Status{
		CreatedAt: time.Now().UnixNano(),
	})
	debris.SandboxID = id
	return tomb, snapshot, debris
}

// TestCompletePendingPausedRestore covers the self-heal for the failover
// stand-down: a rollback that could not take the lifecycle lock recorded the
// restore as pending; the next resume Create completes it under its own lock
// before the gate and admission read the row.
func TestCompletePendingPausedRestore(t *testing.T) {
	ctx := context.Background()

	t.Run("restores the tombstone over the debris row", func(t *testing.T) {
		_, snapshot, debris := pendingRestoreFixtures("sb-pending")
		now := time.Now()
		snapshot.UserMarkDeletedTime = &now // scrubbed on restore
		mgr := &fakeCubeboxAPI{cb: debris}
		l := &local{cubeboxManger: mgr}
		l.recordPendingPausedRestore("sb-pending", snapshot, debris.CreatedAt)

		require.True(t, l.completePendingPausedRestore(ctx, "sb-pending"))
		assert.Equal(t, 1, mgr.saveCalls)
		assert.Zero(t, l.pendingPausedRestores.len(), "the entry is consumed")

		cur, err := mgr.Get(ctx, "sb-pending")
		require.NoError(t, err)
		require.NotNil(t, cur)
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State())
		assert.Nil(t, cur.UserMarkDeletedTime)
		assert.NotNil(t, cur.ResourceWithOverHead)
	})

	t.Run("drops the entry when the row is gone", func(t *testing.T) {
		// A user Delete won the lock first: resurrecting the tombstone would
		// leave a phantom row whose pause binding Master already dropped.
		_, snapshot, debris := pendingRestoreFixtures("sb-pending-gone")
		mgr := &fakeCubeboxAPI{getErr: utils.ErrorKeyNotFound}
		l := &local{cubeboxManger: mgr}
		l.recordPendingPausedRestore("sb-pending-gone", snapshot, debris.CreatedAt)

		assert.False(t, l.completePendingPausedRestore(ctx, "sb-pending-gone"))
		assert.Zero(t, mgr.saveCalls)
		assert.Zero(t, l.pendingPausedRestores.len())
	})

	t.Run("re-saving an intact tombstone is a benign no-op", func(t *testing.T) {
		// The attempt failed before its first Save, so the row is still the
		// tombstone itself and the recorded identity matches: the deferred
		// restore simply Saves the same content back.
		tomb, snapshot, _ := pendingRestoreFixtures("sb-pending-paused")
		mgr := &fakeCubeboxAPI{cb: tomb}
		l := &local{cubeboxManger: mgr}
		l.recordPendingPausedRestore("sb-pending-paused", snapshot, tomb.CreatedAt)

		assert.True(t, l.completePendingPausedRestore(ctx, "sb-pending-paused"))
		assert.Equal(t, 1, mgr.saveCalls)
		cur, err := mgr.Get(ctx, "sb-pending-paused")
		require.NoError(t, err)
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State())
	})

	t.Run("drops the entry when a newer attempt owns the row", func(t *testing.T) {
		_, snapshot, debris := pendingRestoreFixtures("sb-pending-newer")
		newer := newCubeboxWithStatusForTest("sb-pending-newer", cubeboxstore.Status{
			StartedAt: time.Now().UnixNano(),
		})
		newer.SandboxID = "sb-pending-newer"
		newer.CreatedAt = debris.CreatedAt + int64(time.Hour)
		mgr := &fakeCubeboxAPI{cb: newer}
		l := &local{cubeboxManger: mgr}
		l.recordPendingPausedRestore("sb-pending-newer", snapshot, debris.CreatedAt)

		assert.False(t, l.completePendingPausedRestore(ctx, "sb-pending-newer"))
		assert.Zero(t, mgr.saveCalls, "a newer attempt's row must not be clobbered")
		assert.Zero(t, l.pendingPausedRestores.len())
	})

	t.Run("no-op without a recorded restore", func(t *testing.T) {
		l := &local{cubeboxManger: &fakeCubeboxAPI{}}
		assert.False(t, l.completePendingPausedRestore(ctx, "sb-nothing"))
	})
}

// TestPendingRestoreRegistryPrunesExpired bounds the in-memory registry: an
// entry no lifecycle operation ever consumed must not linger forever, and a
// stale entry must not be applied long after Master and the node moved on.
func TestPendingRestoreRegistryPrunesExpired(t *testing.T) {
	ctx := context.Background()
	_, snapshot, debris := pendingRestoreFixtures("sb-expired")
	mgr := &fakeCubeboxAPI{cb: debris}
	l := &local{cubeboxManger: mgr}

	l.pendingPausedRestores.record("sb-expired", snapshot, debris.CreatedAt, time.Now().Add(-2*pendingRestoreTTL))
	assert.False(t, l.completePendingPausedRestore(ctx, "sb-expired"),
		"an expired deferred restore must be dropped, not applied")
	assert.Zero(t, mgr.saveCalls)

	l.pendingPausedRestores.record("sb-a", snapshot, debris.CreatedAt, time.Now())
	l.pendingPausedRestores.record("sb-b", snapshot, debris.CreatedAt, time.Now().Add(-2*pendingRestoreTTL))
	l.pendingPausedRestores.record("sb-c", snapshot, debris.CreatedAt, time.Now())
	assert.Equal(t, 2, l.pendingPausedRestores.len(), "recording prunes expired entries")
}

// TestRetryCompletesDeferredTombstoneRestoreAndIsGated is the end-to-end pin
// for the stand-down self-heal: the first attempt's rollback could not get the
// lifecycle lock and recorded the restore as pending, leaving its debris row;
// the retry then completes the restore under its own lock BEFORE the gate and
// admission run, so the retry is quota-gated on the tombstone instead of
// sliding past both checks against the debris row.
func TestRetryCompletesDeferredTombstoneRestoreAndIsGated(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 0 // isolate the memory dimension
	hostConf.Quota.Mem = "8Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// The retry is a thin Create: expansion needs the pause package on disk.
	seedPausePackageForTest(t, "snap-test", &cubeboxpb.RunCubeSandboxRequest{
		RequestID: "original-create",
		Containers: []*cubeboxpb.ContainerConfig{{
			Name:      "main",
			Resources: &cubeboxpb.Resource{Cpu: "4000m", Mem: "8Gi"},
		}},
	})

	_, snapshot, debris := pendingRestoreFixtures("sb-deferred")
	filler := sandboxWithResourceForTest("sb-filler", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "6Gi", 0, 0, 0)
	filler.SandboxID = "sb-filler"

	mgr := &fakeCubeboxAPI{list: []*cubeboxstore.CubeBox{debris, filler}}
	flow := &fakeCreateFlow{}
	engine := &workflow.Engine{}
	engine.AddFlow("create", &workflow.Workflow{
		Name:    "create",
		Limiter: semaphore.NewLimiter(1),
		Steps: []*workflow.Step{{
			Name:    flow.ID(),
			Actions: []workflow.Flow{flow},
		}},
	})
	l := &local{
		config: &CubeConfig{
			DefaultRuntimeName: "io.containerd.cube.v2.task",
			Runtimes: map[string]cubeconfig.Runtime{
				"io.containerd.cube.v2.task": {Type: "io.containerd.cube.v2.task"},
			},
		},
		cubeboxManger: mgr,
	}
	l.SetShimIntentTTL(time.Minute)
	s := &service{
		engine:                engine,
		cubeboxMgr:            l,
		sandboxLifecycleLocks: utils.NewResourceLocks(),
	}

	// The stood-down rollback left this behind.
	l.recordPendingPausedRestore("sb-deferred", snapshot, debris.CreatedAt)

	rsp, err := s.Create(context.Background(), pausedReplaceRequest("sb-deferred"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp.Ret.RetCode,
		"the retry must be quota-gated on the tombstone the deferred restore put back (6 GiB used + 4 GiB need > 8 GiB)")
	assert.Contains(t, rsp.Ret.RetMsg, "mem quota")
	assert.Zero(t, flow.createCalls, "the rejection happens before the flow allocates anything")
	assert.Zero(t, l.pendingPausedRestores.len(), "the deferred restore was consumed")

	cur, getErr := mgr.Get(context.Background(), "sb-deferred")
	require.NoError(t, getErr)
	require.NotNil(t, cur)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State(),
		"the debris row is gone; the tombstone is back for the next retry too")
}

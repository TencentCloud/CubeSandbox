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
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func rollbackDestroyContext(id string) *workflow.DestroyContext {
	opts := &workflow.DestroyContext{
		DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{
			SandboxID: id,
			RequestID: "failover-rollback",
		},
	}
	opts.SandboxID = id
	opts.IsRollBack = true
	return opts
}

// TestRestorePausedReplaceSnapshotPinsPostSaveRollback covers the failure
// window the keep-the-row guard alone cannot: createContainers Saves the new
// row early, before runContainer/probe/envd init, so a rollback after that
// point finds this attempt's debris (state CREATED/RUNNING), not the PAUSED
// tombstone. The snapshot carried from the replacement branch must be saved
// back so the retry is gated on the original tombstone again.
func TestRestorePausedReplaceSnapshotPinsPostSaveRollback(t *testing.T) {
	tombstone := func(id string) *cubeboxstore.CubeBox {
		sb := sandboxWithResourceForTest(id, cubeboxstore.Status{
			StartedAt: 1,
			PausedAt:  time.Now().UnixNano(),
		}, "0m", "4Gi", 0, 0, 0)
		sb.SandboxID = id
		sb.RootfsWritableLayer = "disk/" + id
		return sb
	}

	t.Run("engine failover after the flow's Save restores the tombstone", func(t *testing.T) {
		tomb := tombstone("sb-postsave")
		// The gc plugin's create step may have stamped the tombstone while the
		// flow ran; the restored row must not read as user-deleted.
		now := time.Now()
		tomb.UserMarkDeletedTime = &now
		snap := tomb.DeepCopy()
		tomb.UserMarkDeletedTime = nil

		// By rollback time the store holds this attempt's debris row: same ID,
		// fresh CreatedAt-only status, no resource metadata yet — exactly what
		// createContainers Saves before runContainer.
		debris := newCubeboxWithStatusForTest("sb-postsave", cubeboxstore.Status{
			CreatedAt: time.Now().UnixNano(),
		})
		debris.SandboxID = "sb-postsave"
		mgr := &fakeCubeboxAPI{cb: debris}
		l := &local{cubeboxManger: mgr}

		opts := rollbackDestroyContext("sb-postsave")
		opts.PausedReplaceSnapshot = snap

		require.True(t, l.restorePausedReplaceSnapshot(context.Background(), opts, "sb-postsave"),
			"a post-Save rollback must restore the snapshotted tombstone instead of deleting the debris")
		assert.Equal(t, 1, mgr.saveCalls, "the restore is exactly one Save over the debris row")
		assert.Zero(t, mgr.deleteCalls, "the restored tombstone must not be deleted")

		cur, err := mgr.Get(context.Background(), "sb-postsave")
		require.NoError(t, err)
		require.NotNil(t, cur)
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State(),
			"the retry's gates key on the PAUSED state")
		assert.Nil(t, cur.UserMarkDeletedTime,
			"the restored tombstone must not read as user-deleted to GC")
		assert.NotNil(t, cur.ResourceWithOverHead, "quota sizing survives the rollback")
		assert.Equal(t, "disk/sb-postsave", cur.RootfsWritableLayer)
	})

	t.Run("envd-init cleanup path restores without IsRollBack", func(t *testing.T) {
		// cleanupAfterEnvdInitFailure marks the failover operation on the
		// context but builds a DestroyContext without IsRollBack; the restore
		// must trigger on either marker.
		tomb := tombstone("sb-envd")
		snap := tomb.DeepCopy()
		mgr := &fakeCubeboxAPI{cb: newCubeboxWithStatusForTest("sb-envd", cubeboxstore.Status{
			StartedAt: time.Now().UnixNano(),
		})}
		l := &local{cubeboxManger: mgr}

		opts := rollbackDestroyContext("sb-envd")
		opts.IsRollBack = false
		opts.PausedReplaceSnapshot = snap
		ctx := constants.WithFailoverOperation(context.Background())

		require.True(t, l.restorePausedReplaceSnapshot(ctx, opts, "sb-envd"))
		cur, err := mgr.Get(context.Background(), "sb-envd")
		require.NoError(t, err)
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State())
	})

	t.Run("a user destroy never restores", func(t *testing.T) {
		// User deletes carry no snapshot (only create-rollback paths set it);
		// even a stale one must not fire without a rollback/failover marker.
		tomb := tombstone("sb-userdel")
		mgr := &fakeCubeboxAPI{cb: newCubeboxWithStatusForTest("sb-userdel", cubeboxstore.Status{
			StartedAt: time.Now().UnixNano(),
		})}
		l := &local{cubeboxManger: mgr}

		opts := rollbackDestroyContext("sb-userdel")
		opts.IsRollBack = false
		opts.PausedReplaceSnapshot = tomb.DeepCopy()

		assert.False(t, l.restorePausedReplaceSnapshot(context.Background(), opts, "sb-userdel"),
			"an ordinary destroy must not resurrect a snapshot it was not rolled back from")
		assert.Zero(t, mgr.saveCalls)
	})

	t.Run("no snapshot falls through to the existing guards", func(t *testing.T) {
		mgr := &fakeCubeboxAPI{cb: newCubeboxWithStatusForTest("sb-plain", cubeboxstore.Status{
			StartedAt: time.Now().UnixNano(),
		})}
		l := &local{cubeboxManger: mgr}

		assert.False(t, l.restorePausedReplaceSnapshot(
			context.Background(), rollbackDestroyContext("sb-plain"), "sb-plain"))
		assert.Zero(t, mgr.saveCalls)
	})
}

// TestCreateRollbackKeepsPausedTombstone pins the delete-timing fix for the
// retry-after-failure bypass: Create-from-pause no longer deletes the PAUSED
// row up front, so when a create fails before its Save, the failover rollback
// must find the tombstone still PAUSED and keep it — deleting it would leave
// the user's retried Resume (Master keeps the pause binding READY) arriving as
// a thin Create with no local row, admitted with no quota check at all.
func TestCreateRollbackKeepsPausedTombstone(t *testing.T) {
	tombstone := func(id string) *cubeboxstore.CubeBox {
		sb := sandboxWithResourceForTest(id, cubeboxstore.Status{
			StartedAt: 1,
			PausedAt:  time.Now().UnixNano(),
			Removing:  true, // the rollback Destroy body stamps this before the guard
		}, "0m", "4Gi", 0, 0, 0)
		sb.SandboxID = id
		return sb
	}

	t.Run("rollback keeps a still-paused tombstone and cleans it up", func(t *testing.T) {
		sb := tombstone("sb-rollback-keep")
		// The rollback Destroy body marks the row user-deleted before the
		// guard runs (destroy.go stamps it for the CDP hooks); the kept
		// tombstone must not carry that mark into the retry window.
		now := time.Now()
		sb.UserMarkDeletedTime = &now
		sb.DeleteRequestID = "failover-rollback"
		mgr := &fakeCubeboxAPI{cb: sb}
		l := &local{cubeboxManger: mgr}

		kept := l.createRollbackKeepsPausedTombstone(
			context.Background(), rollbackDestroyContext("sb-rollback-keep"), "sb-rollback-keep")

		require.True(t, kept, "a create rollback must not consume the paused tombstone")
		assert.Zero(t, mgr.deleteCalls, "the guard keeps the row without touching Delete")

		cur, err := mgr.Get(context.Background(), "sb-rollback-keep")
		require.NoError(t, err)
		require.NotNil(t, cur)
		assert.Nil(t, cur.UserMarkDeletedTime,
			"the kept tombstone must not read as user-deleted (GC would proceed against it)")
		assert.Empty(t, cur.DeleteRequestID)
		st := cur.GetStatus().Get()
		assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, st.State(),
			"the retry's gate keys on the PAUSED state")
		assert.False(t, st.Removing, "the destroy body's Removing mark must be scrubbed")
		assert.NotZero(t, st.PausedAt)
		assert.Contains(t, mgr.syncIDs, "sb-rollback-keep", "the scrubbed tombstone must be persisted")
	})

	t.Run("rollback deletes a row the failed recreate already saved", func(t *testing.T) {
		// If the flow reached its Save the row is this attempt's debris, not
		// resumable state: the deferred delete must proceed as usual.
		saved := newCubeboxWithStatusForTest("sb-rollback-saved", cubeboxstore.Status{
			StartedAt: time.Now().UnixNano(),
		})
		saved.SandboxID = "sb-rollback-saved"
		l := &local{cubeboxManger: &fakeCubeboxAPI{cb: saved}}

		assert.False(t, l.createRollbackKeepsPausedTombstone(
			context.Background(), rollbackDestroyContext("sb-rollback-saved"), "sb-rollback-saved"))
	})

	t.Run("a user destroy of a paused sandbox still deletes it", func(t *testing.T) {
		// The guard is rollback-only: Master's tombstone drop and user deletes
		// are not rollbacks and must not be blocked by it.
		sb := tombstone("sb-user-destroy")
		l := &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}
		opts := rollbackDestroyContext("sb-user-destroy")
		opts.IsRollBack = false

		assert.False(t, l.createRollbackKeepsPausedTombstone(context.Background(), opts, "sb-user-destroy"))
	})

	t.Run("a missing or unreadable row keeps the old behaviour", func(t *testing.T) {
		l := &local{cubeboxManger: &fakeCubeboxAPI{getErr: utils.ErrorKeyNotFound}}
		assert.False(t, l.createRollbackKeepsPausedTombstone(
			context.Background(), rollbackDestroyContext("sb-gone"), "sb-gone"),
			"nothing to keep: the deferred delete proceeds exactly as before")
	})
}

// TestRetryAfterFailedResumeIsStillQuotaGated is the end-to-end pin the
// delete-timing fix exists for: a resume that fit when it was admitted, then
// failed mid-flow, leaves its tombstone behind; if the node fills up before
// the user retries, the retry must be re-checked and rejected — not waved
// through as a plain create because its local record vanished with the first
// attempt (the bypass reported against the up-front row delete).
func TestRetryAfterFailedResumeIsStillQuotaGated(t *testing.T) {
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

	tomb := sandboxWithResourceForTest("sb-retry", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-retry"
	mgr := &fakeCubeboxAPI{list: []*cubeboxstore.CubeBox{tomb}}
	s := &service{cubeboxMgr: &local{cubeboxManger: mgr}}
	ctx := context.Background()

	// Attempt 1 fits (0 used + 4 GiB need < 8 GiB) and is admitted.
	release, rejected := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-retry"))
	require.Nil(t, rejected, "the first attempt must be admitted while the node has room")
	// The create flow then fails before its Save: Create's deferred release
	// fires, and the failover rollback runs against the still-PAUSED row.
	release()
	require.True(t, s.cubeboxMgr.createRollbackKeepsPausedTombstone(
		ctx, rollbackDestroyContext("sb-retry"), "sb-retry"),
		"the failed attempt must roll back to a tombstone the retry can gate on")
	require.Zero(t, mgr.deleteCalls, "the tombstone must survive the rollback")

	// Before the user retries, an unrelated sandbox takes the capacity.
	other := sandboxWithResourceForTest("sb-other", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "6Gi", 0, 0, 0)
	other.SandboxID = "sb-other"
	mgr.list = append(mgr.list, other)

	// Attempt 2: the same thin Create must now be refused — 6 GiB used plus
	// the 4 GiB the resume re-adds no longer fits the 8 GiB node. With the
	// tombstone deleted (the old timing) this retry resolved to "not a
	// replacement" and was admitted with no quota check at all.
	release2, rejected2 := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-retry"))
	require.NotNil(t, rejected2, "the retry must be gated on the tombstone the rollback kept")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
	assert.Contains(t, rejected2.RetMsg, "mem quota")
	release2()

	memMB, _ := s.cubeboxMgr.resumeReservations.totals()
	assert.Zero(t, memMB, "a rejected retry must not leave a reservation behind")
}

// TestRetryAfterPostSaveFailureIsStillQuotaGated is the end-to-end pin for the
// window the tombstone-keep guard alone cannot cover: createContainers Saves
// the new row early — before runContainer, probe and envd init, the common
// failure points — so by rollback time the PAUSED tombstone is already
// overwritten by this attempt's debris. Without the snapshot restore, the
// rollback would delete that debris and the retry would arrive with no local
// row, bypassing the quota gate again.
func TestRetryAfterPostSaveFailureIsStillQuotaGated(t *testing.T) {
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

	tomb := sandboxWithResourceForTest("sb-postsaveretry", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-postsaveretry"
	mgr := &fakeCubeboxAPI{list: []*cubeboxstore.CubeBox{tomb}}
	s := &service{cubeboxMgr: &local{cubeboxManger: mgr}}
	ctx := context.Background()

	// Attempt 1: admitted, then the replacement branch snapshots the tombstone
	// and the flow's early Save overwrites the row with a fresh CREATED-only
	// record before the step that fails (runContainer / probe / envd init).
	release, rejected := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-postsaveretry"))
	require.Nil(t, rejected, "the first attempt must be admitted while the node has room")
	snapshot := tomb.DeepCopy()
	debris := newCubeboxWithStatusForTest("sb-postsaveretry", cubeboxstore.Status{
		CreatedAt: time.Now().UnixNano(),
	})
	debris.SandboxID = "sb-postsaveretry"
	require.NoError(t, mgr.Save(ctx, debris), "the flow's early Save overwrites the tombstone by ID")
	release()

	// The failover rollback finds the debris row, not a PAUSED one: only the
	// snapshot restore can put the tombstone back.
	rollback := rollbackDestroyContext("sb-postsaveretry")
	rollback.PausedReplaceSnapshot = snapshot
	require.False(t, s.cubeboxMgr.createRollbackKeepsPausedTombstone(ctx, rollback, "sb-postsaveretry"),
		"the debris row is not PAUSED — the keep-guard must defer to the snapshot restore")
	require.True(t, s.cubeboxMgr.restorePausedReplaceSnapshot(ctx, rollback, "sb-postsaveretry"),
		"the rollback must restore the snapshotted tombstone instead of deleting the debris")

	// Before the retry, the node fills up.
	other := sandboxWithResourceForTest("sb-other2", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "6Gi", 0, 0, 0)
	other.SandboxID = "sb-other2"
	mgr.list = append(mgr.list, other)

	// Attempt 2: the same thin Create, gated on the restored tombstone.
	release2, rejected2 := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-postsaveretry"))
	require.NotNil(t, rejected2, "the post-Save-failure retry must be gated on the restored tombstone")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
	assert.Contains(t, rejected2.RetMsg, "mem quota")
	release2()
}

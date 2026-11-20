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

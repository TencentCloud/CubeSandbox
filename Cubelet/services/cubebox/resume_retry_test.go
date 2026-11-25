// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// TestCreateResolveErrorTriggersRollbackNotStrand pins the error-code choice
// for a store read failure inside local.Create's replacement branch: the code
// must reach a rollback owner (i.e. must NOT be PreConditionFailed — the one
// code both the engine's async failover and service.Create's synchronous
// rollback skip). cubebox is the create flow's last step, so
// network/volume/storage/cgroup are already allocated at this point —
// skipping the rollback would strand them until a user DELETE, while the
// rollback releases them and restores the still-PAUSED tombstone.
func TestCreateResolveErrorTriggersRollbackNotStrand(t *testing.T) {
	l := &local{cubeboxManger: &fakeCubeboxAPI{getErr: errors.New("boltdb: i/o error")}}
	opts := &workflow.CreateContext{ReqInfo: resumeReplaceRequest("sb-resolve-err")}

	err := l.Create(context.Background(), opts)

	require.Error(t, err)
	assert.False(t, ret.IsErrorCode(err, errorcode.ErrorCode_PreConditionFailed),
		"PreConditionFailed would skip the rollback and strand the earlier steps' allocations")
	assert.True(t, ret.IsErrorCode(err, errorcode.ErrorCode_LoadContainerFromMetaDataFailed),
		"the store read failure must surface as a rollback-triggering metadata error")
	assert.Contains(t, err.Error(), "cannot resolve the paused sandbox to replace")
}

// TestRetryAfterFailedResumeIsStillQuotaGated is the end-to-end pin the
// no-delete contract exists for: a resume that fit when it was admitted, then
// failed before its first Save, leaves its tombstone behind (the rollback
// destroy keeps the row and the synchronous restore saves the gate's snapshot
// back over it); if the node fills up before the user retries, the retry must
// be re-checked and rejected — not waved through as a plain create because its
// local record vanished with the first attempt (the bypass reported against
// the up-front row delete).
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

	// Attempt 1 fits (0 used + 4 GiB need < 8 GiB) and is admitted. The gate
	// captured the tombstone snapshot before the flow ran.
	release, rejected := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-retry"))
	require.Nil(t, rejected, "the first attempt must be admitted while the node has room")
	snapshot := tomb.DeepCopy()
	// The flow fails before its Save; the rollback destroy keeps the row and
	// the synchronous restore saves the snapshot back, marks scrubbed.
	now := time.Now()
	snapshot.UserMarkDeletedTime = &now
	snapshot.DeleteRequestID = "attempt-1"
	s.restorePausedTombstone(ctx, snapshot)
	release()

	require.Zero(t, mgr.deleteCalls, "the rollback must never delete the tombstone row")
	restored, gerr := mgr.Get(ctx, "sb-retry")
	require.NoError(t, gerr)
	require.NotNil(t, restored)
	assert.Nil(t, restored.UserMarkDeletedTime,
		"the restored tombstone must not read as user-deleted")
	assert.Empty(t, restored.DeleteRequestID)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State(),
		"the retry's gate keys on the PAUSED state")

	// Before the user retries, an unrelated sandbox takes the capacity.
	other := sandboxWithResourceForTest("sb-other", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "6Gi", 0, 0, 0)
	other.SandboxID = "sb-other"
	mgr.list = append(mgr.list, other)
	// Drop the admission aggregate cache so the retry's check sees the new
	// row (the TTL window would otherwise serve the first attempt's view).
	s.cubeboxMgr.aggregateCache = aggregateCache{}

	// Attempt 2: the same thin Create must now be refused — 6 GiB used plus
	// the 4 GiB the resume re-adds no longer fits the 8 GiB node. With the
	// tombstone deleted (the old timing) this retry resolved to "not a
	// replacement" and was admitted with no quota check at all.
	release2, rejected2 := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-retry"))
	require.NotNil(t, rejected2, "the retry must be gated on the tombstone the rollback kept")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
	assert.Contains(t, rejected2.RetMsg, "mem quota")
	release2()

	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "a rejected retry must not leave a reservation behind")
}

// TestRetryAfterPostSaveFailureIsStillQuotaGated is the end-to-end pin for
// the window the row-keep alone cannot cover: createContainers Saves the new
// row early — before runContainer, probe and envd init, the common failure
// points — so by rollback time the PAUSED tombstone is already overwritten by
// this attempt's debris. The synchronous restore saves the gate's snapshot
// back over that debris; without it the retry would arrive with no local
// PAUSED row, bypassing the quota gate again.
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

	// Attempt 1: admitted, the gate snapshots the tombstone, and the flow's
	// early Save overwrites the row with a fresh CREATED-only record before
	// the step that fails (runContainer / probe / envd init).
	release, rejected := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-postsaveretry"))
	require.Nil(t, rejected, "the first attempt must be admitted while the node has room")
	snapshot := tomb.DeepCopy()
	debris := newCubeboxWithStatusForTest("sb-postsaveretry", cubeboxstore.Status{
		CreatedAt: time.Now().UnixNano(),
	})
	debris.SandboxID = "sb-postsaveretry"
	require.NoError(t, mgr.Save(ctx, debris), "the flow's early Save overwrites the tombstone by ID")
	release()

	// The rollback destroy kept the debris row (RollbackKeepsRow); the
	// synchronous restore saves the snapshot back over it.
	s.restorePausedTombstone(ctx, snapshot)

	restored, gerr := mgr.Get(ctx, "sb-postsaveretry")
	require.NoError(t, gerr)
	require.NotNil(t, restored)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State(),
		"the restore must put the tombstone back over the debris row")
	assert.Equal(t, tomb.CreatedAt, restored.CreatedAt,
		"the restored row is the pre-attempt tombstone, not the debris")

	// Before the retry, the node fills up.
	other := sandboxWithResourceForTest("sb-other2", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "6Gi", 0, 0, 0)
	other.SandboxID = "sb-other2"
	mgr.list = append(mgr.list, other)
	s.cubeboxMgr.aggregateCache = aggregateCache{} // as above: the retry must see the new row

	// Attempt 2: the same thin Create, gated on the restored tombstone.
	release2, rejected2 := s.admitPausedReplace(ctx, pausedReplaceRequest("sb-postsaveretry"))
	require.NotNil(t, rejected2, "the post-Save-failure retry must be gated on the restored tombstone")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
	assert.Contains(t, rejected2.RetMsg, "mem quota")
	release2()
}

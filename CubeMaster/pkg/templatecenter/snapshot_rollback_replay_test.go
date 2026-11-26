// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
)

func TestRollbackReadyReplayWhileLifecycleLockHeld(t *testing.T) {
	ctx := t.Context()
	db := newMigrateSharedStoreTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&models.TemplateImageJob{}, &models.TemplateReplica{}, &models.SnapshotRecord{}))

	const sandboxID, snapshotID = "sb-rollback", "snap-rollback"
	payload, err := marshalSnapshotRollbackRequest("req-ready", sandboxID, snapshotID, "node", "127.0.0.1", 1, 0, "xfs")
	require.NoError(t, err)
	ready := models.TemplateImageJob{
		JobID: "job-ready", RequestID: "req-ready", SandboxID: sandboxID, TemplateID: snapshotID,
		Operation: JobOperationSnapshotRollback, Status: JobStatusReady, RequestJSON: payload,
	}
	require.NoError(t, db.Create(&ready).Error)
	require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: snapshotID, Status: StatusReady}).Error)
	replica := models.TemplateReplica{TemplateID: snapshotID, NodeID: "node", NodeIP: "127.0.0.1", Status: ReplicaStatusReady}
	require.NoError(t, db.Create(&replica).Error)
	payload, err = marshalSnapshotRollbackRequest("req-pending", sandboxID, snapshotID, "node", "127.0.0.1", 1, 0, "xfs")
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID: "job-pending", RequestID: "req-pending", SandboxID: sandboxID, TemplateID: snapshotID,
		Operation: JobOperationSnapshotRollback, Status: JobStatusPending, RequestJSON: payload,
	}).Error)

	redisServer := miniredis.RunT(t)
	redis := wrapredis.GetRedisConnPoolWrap("test-rollback-ready-replay", &config.RedisConf{
		Nodes: redisServer.Addr(), MaxActive: 4, MaxIdle: 2, MaxRetry: 1,
	})
	t.Cleanup(func() { require.NoError(t, redis.RedisConnPool.Close()) })
	patches := gomonkey.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return redis })
	t.Cleanup(patches.Reset)
	// Another operation holds the shared lifecycle lock for this sandbox.
	require.NoError(t, redisServer.Set(rediskey.LockSandbox(sandboxID), "other-operation"))

	info, err := RollbackSandboxToSnapshot(ctx, ready.RequestID, sandboxID, snapshotID, "cubebox", "")
	require.NoError(t, err)
	require.Equal(t, ready.JobID, info.JobID)
	require.Equal(t, JobStatusReady, info.Status)
	require.NoError(t, db.Delete(&replica).Error)
	info, err = RollbackSandboxToSnapshot(ctx, ready.RequestID, sandboxID, snapshotID, "cubebox", "")
	require.NoError(t, err)
	require.Equal(t, ready.JobID, info.JobID)

	for _, tc := range []struct{ requestID, target string }{
		{"req-new", snapshotID},
		{"req-pending", snapshotID},
		{ready.RequestID, "snap-other"},
	} {
		waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		info, err := RollbackSandboxToSnapshot(waitCtx, tc.requestID, sandboxID, tc.target, "cubebox", "")
		cancel()
		require.ErrorIs(t, err, sandboxlock.ErrLockNotAcquired)
		require.Nil(t, info)
	}
	redisServer.Del(rediskey.LockSandbox(sandboxID))
	_, err = RollbackSandboxToSnapshot(ctx, ready.RequestID, sandboxID, "snap-other", "cubebox", "")
	require.ErrorIs(t, err, ErrTemplateAttemptInProgress)
	require.ErrorContains(t, err, "payload does not match")
}

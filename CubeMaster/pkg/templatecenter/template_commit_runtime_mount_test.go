// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
)

func TestTemplateCommitOrdersRuntimeAttach(t *testing.T) {
	db := snapshotReservationTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&models.PauseSnapshotRecord{}, &models.VolumeRecord{}, &models.RuntimeVolumeAttach{}, &models.TemplateReplica{}))
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_commit_spec_sandbox ON t_cube_sandbox_spec(sandbox_id)").Error)
	require.NoError(t, sandboxspec.Init(db))
	volume := models.VolumeRecord{VolumeID: "dataset", Name: "dataset", Driver: "s3"}
	require.NoError(t, db.Create(&volume).Error)

	server := miniredis.RunT(t)
	redis := wrapredis.GetRedisConnPoolWrap("test-commit-runtime-attach", &config.RedisConf{Nodes: server.Addr(), MaxActive: 4, MaxIdle: 2, MaxRetry: 1})
	t.Cleanup(func() { require.NoError(t, redis.RedisConnPool.Close()) })
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return redis })
	patches.ApplyFunc(dao.Default, func() *gorm.DB { return db })
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error { return nil })
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList { return nil })
	patches.ApplyFunc(localcache.RegisterTemplateReplica, func(string, string, int64) {})

	var captures atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	staleEntered, staleRelease := make(chan struct{}, 1), make(chan struct{})
	patches.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, ip string, req *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
		captures.Add(1)
		switch req.GetTemplateID() {
		case "tpl-first":
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "tpl-stale":
			staleEntered <- struct{}{}
			<-staleRelease
		}
		return okCommit(ip)
	})
	cleanups := 0
	patches.ApplyFunc(cubelet.CleanupTemplate, func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		cleanups++
		return successCleanup(), nil
	})
	newSandbox := func(t *testing.T, id string) {
		t.Helper()
		require.NoError(t, sandboxspec.Put(t.Context(), id, &sandboxtypes.CreateCubeSandboxReq{
			InstanceType: "cubebox", Containers: []*sandboxtypes.Container{{Name: "main"}},
		}, sandboxspec.PutOptions{}))
	}
	attachDispatches := 0
	attach := func(ctx context.Context, id string, outcome sandbox.RuntimeAttachOutcome) error {
		req := sandbox.RuntimeAttachRequest{SandboxID: id, RequestID: "attach", ExecutionID: "vm", VolumeID: volume.VolumeID, ContainerPath: "/data", Readonly: true}
		return sandbox.AttachRuntimeVolume(ctx, req, EnsureNoActiveSnapshotJobInTx, func(_ context.Context, binding sandbox.RuntimeAttachBinding, _ models.VolumeRecord) (sandbox.RuntimeAttachReply, error) {
			attachDispatches++
			event := []byte(`[{"volume_id":"dataset","referenced":1}]`)
			return sandbox.RuntimeAttachReply{Binding: binding, Outcome: outcome, ExtInfo: map[string][]byte{refcount.ExtInfoKey: event}}, nil
		})
	}
	waitReady := func(t *testing.T, id string) *models.TemplateDefinition {
		t.Helper()
		require.Eventually(t, func() bool {
			job, err := getTemplateImageJobByRequestOperation(t.Context(), id, JobOperationCommit)
			return err == nil && (job.Status == JobStatusReady || job.Status == JobStatusFailed)
		}, 5*time.Second, 10*time.Millisecond)
		job, err := getTemplateImageJobByRequestOperation(t.Context(), id, JobOperationCommit)
		require.NoError(t, err)
		require.Equal(t, JobStatusReady, job.Status, job.ErrorMessage)
		def, err := GetDefinition(t.Context(), job.TemplateID)
		require.NoError(t, err)
		return def
	}
	for _, tc := range []struct {
		name     string
		outcome  sandbox.RuntimeAttachOutcome
		override *sandboxtypes.CreateCubeSandboxReq
	}{
		{"pending", sandbox.RuntimeAttachUnknown, nil},
		{"confirmed", sandbox.RuntimeAttachPublished, nil},
		{"confirmed-override", sandbox.RuntimeAttachPublished, &sandboxtypes.CreateCubeSandboxReq{InstanceType: "cubebox"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "sb-" + tc.name
			newSandbox(t, id)
			ctx := t.Context()
			err := attach(ctx, id, tc.outcome)
			if tc.outcome == sandbox.RuntimeAttachUnknown {
				require.ErrorIs(t, err, sandbox.ErrRuntimeAttachUnresolved)
			} else {
				require.NoError(t, err)
			}
			before := captures.Load()
			_, err = SubmitTemplateCommit(ctx, id, id, "host", "127.0.0.1", "tpl-"+tc.name, tc.override)
			if err == nil {
				def := waitReady(t, id)
				t.Errorf("unexpected %s template: capture RPCs=%d, contains dataset=%t", def.Status, captures.Load()-before, strings.Contains(def.RequestJSON, volume.VolumeID))
			}
			require.ErrorIs(t, err, ErrTemplateAttemptInProgress)
			if tc.outcome == sandbox.RuntimeAttachUnknown {
				require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
			} else {
				require.ErrorIs(t, err, ErrTemplateCommitRuntimeMounts)
			}
			_, err = getTemplateImageJobByRequestOperation(ctx, id, JobOperationCommit)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)
			require.Equal(t, before, captures.Load())
		})
	}
	t.Run("rejected retry preserves failed template", func(t *testing.T) {
		ctx := t.Context()
		const id, templateID = "sb-retry", "tpl-retry"
		newSandbox(t, id)
		require.ErrorIs(t, attach(ctx, id, sandbox.RuntimeAttachUnknown), sandbox.ErrRuntimeAttachUnresolved)
		failed := models.TemplateDefinition{TemplateID: templateID, Status: StatusFailed, LastError: "previous build failed"}
		require.NoError(t, db.Create(&failed).Error)
		replica := models.TemplateReplica{TemplateID: templateID, NodeID: "old-node", NodeIP: "127.0.0.1", CleanupRequired: true}
		require.NoError(t, db.Create(&replica).Error)
		before := cleanups
		_, err := SubmitTemplateCommit(ctx, "retry", id, "host", "127.0.0.1", templateID, nil)
		require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
		require.Equal(t, before, cleanups)
		retained, err := GetDefinition(ctx, templateID)
		require.NoError(t, err)
		require.Equal(t, failed.ID, retained.ID)
		require.Equal(t, failed.LastError, retained.LastError)
		require.NoError(t, db.First(&replica, replica.ID).Error)
		require.True(t, replica.CleanupRequired)
	})
	for _, retry := range []struct {
		name       string
		cleanupErr error
	}{
		{"clean old replica", nil},
		{"old replica cleanup fails", errors.New("node unavailable")},
	} {
		t.Run(retry.name, func(t *testing.T) {
			ctx := t.Context()
			id := strings.ReplaceAll(retry.name, " ", "-")
			templateID := "tpl-" + id
			newSandbox(t, id)
			require.NoError(t, db.Create(&models.TemplateDefinition{TemplateID: templateID, Status: StatusFailed}).Error)
			require.NoError(t, db.Create(&models.TemplateReplica{TemplateID: templateID, NodeID: "old-host", NodeIP: "127.0.0.2"}).Error)
			oldRPC := cleanupTemplateOnCubelet
			t.Cleanup(func() { cleanupTemplateOnCubelet = oldRPC })
			var contacted []string
			cleanupTemplateOnCubelet = func(_ context.Context, ep string, _ *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
				contacted = append(contacted, ep)
				return successCleanup(), retry.cleanupErr
			}
			before := captures.Load()
			_, err := SubmitTemplateCommit(ctx, id, id, "host", "127.0.0.1", templateID, nil)
			if retry.cleanupErr == nil {
				require.NoError(t, err)
				waitReady(t, id)
				require.Equal(t, before+1, captures.Load())
			} else {
				require.ErrorIs(t, err, retry.cleanupErr)
				job, err := getTemplateImageJobByRequestOperation(ctx, id, JobOperationCommit)
				require.NoError(t, err)
				require.Equal(t, JobStatusFailed, job.Status)
				require.Equal(t, before, captures.Load())
				require.NoError(t, attach(ctx, id, sandbox.RuntimeAttachPublished))
			}
			require.Equal(t, []string{getCubeletAddrForDelete("127.0.0.2")}, contacted)
		})
	}
	t.Run("commit first", func(t *testing.T) {
		ctx := t.Context()
		const id = "sb-commit-first"
		newSandbox(t, id)
		job, err := SubmitTemplateCommit(ctx, id, id, "host", "127.0.0.1", "tpl-first", nil)
		require.NoError(t, err)
		finish := sync.OnceFunc(func() {
			close(release)
			waitReady(t, id)
		})
		defer finish()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("commit worker did not reach capture")
		}
		before := attachDispatches
		server.FastForward(2 * snapshotOperationTimeout)
		err = attach(ctx, id, sandbox.RuntimeAttachPublished)
		require.ErrorIs(t, err, ErrTemplateAttemptInProgress, "Attach dispatched %d time(s) during capture", attachDispatches-before)
		require.Equal(t, before, attachDispatches)
		finish()
		require.NoError(t, attach(ctx, id, sandbox.RuntimeAttachPublished))
		require.Equal(t, before+1, attachDispatches)
		captured := captures.Load()
		replay, err := SubmitTemplateCommit(ctx, id, id, "host", "127.0.0.1", "tpl-first", nil)
		require.NoError(t, err)
		require.Equal(t, job.JobID, replay.JobID)
		require.Equal(t, JobStatusReady, replay.Status)
		require.Equal(t, captured, captures.Load())
	})
	t.Run("worker loses its job during capture", func(t *testing.T) {
		ctx := t.Context()
		const id, templateID, jobID = "sb-stale", "tpl-stale", "stale-job"
		newSandbox(t, id)
		require.NoError(t, db.Create(&models.TemplateImageJob{JobID: jobID, SandboxID: id, TemplateID: templateID,
			Operation: JobOperationCommit, Status: JobStatusPending, Phase: JobPhaseSnapshotting}).Error)
		create, stored := newCommitFixtureRequests()
		create.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
		run := func() { runTemplateCommitJob(ctx, jobID, id, "host", "127.0.0.1", create, stored) }
		done := make(chan struct{})
		go func() { defer close(done); run() }()
		finish := sync.OnceFunc(func() {
			close(staleRelease)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stale worker did not finish")
			}
		})
		defer finish()
		select {
		case <-staleEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not reach capture")
		}
		require.NoError(t, updateTemplateImageJob(ctx, jobID, map[string]any{"status": JobStatusFailed}))
		require.NoError(t, attach(ctx, id, sandbox.RuntimeAttachPublished))
		before := cleanups
		finish()
		job, err := getTemplateImageJobRecordByID(ctx, jobID)
		require.NoError(t, err)
		require.Equal(t, JobStatusFailed, job.Status)
		_, err = GetDefinition(ctx, templateID)
		require.ErrorIs(t, err, ErrTemplateNotFound)
		require.Equal(t, before, cleanups)
		dispatched := captures.Load()
		run()
		require.Equal(t, dispatched, captures.Load(), "a worker starting after the terminal decision must not capture")
	})
}

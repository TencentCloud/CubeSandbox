// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
	"gorm.io/gorm"
)

func TestSnapshotCaptureFreezesRuntimeMountsInLifecycleOrder(t *testing.T) {
	db := snapshotReservationTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&models.SandboxSpec{}, &models.PauseSnapshotRecord{}, &models.VolumeRecord{}, &models.RuntimeVolumeAttach{}))
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_capture_spec_sandbox ON t_cube_sandbox_spec(sandbox_id)").Error)
	require.NoError(t, sandboxspec.Init(db))
	pausesnap.Init(db)
	t.Cleanup(func() { pausesnap.Init(nil) })
	redisServer := miniredis.RunT(t)
	redis := wrapredis.GetRedisConnPoolWrap("test-snapshot-capture-order", &config.RedisConf{
		Nodes: redisServer.Addr(), MaxActive: 4, MaxIdle: 2, MaxRetry: 1,
	})
	t.Cleanup(func() { require.NoError(t, redis.RedisConnPool.Close()) })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return redis })
	patches.ApplyFunc(dao.Default, func() *gorm.DB { return db })
	patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	patches.ApplyFunc(ensureSnapshotNodeWritable, func(context.Context, string, string, bool) error { return nil })
	patches.ApplyFunc(originHostFactsJSON, func(context.Context, string) string { return "" })
	patches.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error { return nil })
	patches.ApplyFunc(registerTemplateReplicaForSnapshot, func(string, string, int64) {})
	patches.ApplyFunc(setTemplateLocalityCache, func(string, []ReplicaStatus) {})
	patches.ApplyFunc(setTemplateRequestCache, func(string, *sandboxtypes.CreateCubeSandboxReq) error { return nil })

	ctx := t.Context()
	const sandboxID = "sb-runtime-snapshot"
	require.NoError(t, sandboxspec.Put(ctx, sandboxID, &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Volumes:      []*sandboxtypes.Volume{{Name: "work"}},
		Containers:   []*sandboxtypes.Container{{Name: "main"}},
		Annotations: map[string]string{
			sandbox.AnnotationPluginVolumeMounts: `[ {"name":"work","container_path":"/work","readonly":true} ]`,
		},
	}, sandboxspec.PutOptions{}))
	volume := models.VolumeRecord{VolumeID: "dataset", Name: "dataset", Driver: "s3"}
	require.NoError(t, db.Create(&volume).Error)
	attachReq := sandbox.RuntimeAttachRequest{
		SandboxID: sandboxID, RequestID: "attach", ExecutionID: "vm", VolumeID: volume.VolumeID, ContainerPath: "/data", Readonly: true,
	}
	attachDispatches := 0
	attach := func(ctx context.Context) error {
		return sandbox.AttachRuntimeVolume(ctx, attachReq, EnsureNoActiveSnapshotJobInTx, func(_ context.Context, binding sandbox.RuntimeAttachBinding, _ models.VolumeRecord) (sandbox.RuntimeAttachReply, error) {
			attachDispatches++
			event := []byte(`[{"volume_id":"dataset","referenced":1}]`)
			return sandbox.RuntimeAttachReply{Binding: binding, Outcome: sandbox.RuntimeAttachPublished, ExtInfo: map[string][]byte{refcount.ExtInfoKey: event}}, nil
		})
	}
	entered, release := make(chan string, 2), make(chan struct{}, 1)
	patches.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, _ string, req *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
		entered <- req.GetTemplateID()
		if req.GetSnapshotMode() == snapshotmode.Full.String() {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &cubeboxv1.CommitSandboxResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}, SnapshotMode: req.GetSnapshotMode()}, nil
	})
	beforeResult := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("snapshot worker did not finish")
		}
	})
	go func() {
		defer close(finished)
		_, err := SubmitSandboxSnapshot(ctx, "before", sandboxID, "host", "127.0.0.1", "before", "", snapshotmode.Full)
		beforeResult <- err
	}()
	var beforeID string
	select {
	case beforeID = <-entered:
	case err := <-beforeResult:
		t.Fatalf("snapshot returned before capture: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not reach capture")
	}
	require.True(t, redisServer.Exists(rediskey.LockSandbox(sandboxID)))
	wait, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	require.ErrorIs(t, attach(wait), sandboxlock.ErrLockNotAcquired)
	stop()
	redisServer.FastForward(2 * snapshotOperationTimeout)
	require.ErrorIs(t, attach(ctx), ErrTemplateAttemptInProgress)
	require.Zero(t, attachDispatches)
	release <- struct{}{}
	select {
	case err := <-beforeResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not finish")
	}

	require.NoError(t, attach(ctx))
	require.Equal(t, 1, attachDispatches)
	after, err := SubmitSandboxSnapshot(ctx, "after", sandboxID, "host", "127.0.0.1", "after", "", snapshotmode.FsOnly)
	require.NoError(t, err)
	select {
	case captured := <-entered:
		require.Equal(t, after.TemplateID, captured)
	default:
		t.Fatal("post-attach snapshot did not reach capture")
	}
	before, err := LookupReadySnapshotCreateReplay(ctx, "before", sandboxID, "before", snapshotmode.Full, 0)
	require.NoError(t, err)
	require.Equal(t, beforeID, before.TemplateID)

	readFrozen := func(id string) *sandboxtypes.CreateCubeSandboxReq {
		rec, err := getSnapshotRecord(ctx, id)
		require.NoError(t, err)
		req, err := requestFromSnapshotJSON(rec.RequestJSON)
		require.NoError(t, err)
		return req
	}
	oldSpec := readFrozen(beforeID)
	require.NotContains(t, oldSpec.Annotations[sandbox.AnnotationPluginVolumeMounts], "dataset")
	require.Empty(t, oldSpec.Annotations[constants.CubeAnnotationSnapshotRuntimeVolumeMounts])
	newSpec := readFrozen(after.TemplateID)
	require.Contains(t, newSpec.Annotations[sandbox.AnnotationPluginVolumeMounts], "dataset")
	var frozen []sandboxspec.RuntimeVolumeMount
	require.NoError(t, json.Unmarshal([]byte(newSpec.Annotations[constants.CubeAnnotationSnapshotRuntimeVolumeMounts]), &frozen))
	require.Equal(t, []sandboxspec.RuntimeVolumeMount{{VolumeID: "dataset", VolumeRecordID: volume.ID, ContainerPath: "/data", Readonly: true}}, frozen)
	stored, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, sandboxID)
	require.NoError(t, err)
	require.NoError(t, sandboxspec.RecordRuntimeVolumeMount(ctx, sandboxID, stored.RowID, sandboxspec.RuntimeVolumeMount{
		VolumeID: "output", VolumeRecordID: volume.ID + 1, ContainerPath: "/output", Readonly: false,
	}))
	_, err = SubmitSandboxSnapshot(ctx, "writable", sandboxID, "host", "127.0.0.1", "writable", "", snapshotmode.FsOnly)
	require.ErrorIs(t, err, ErrFilesystemOnlySharedVolume)
	_, err = getTemplateImageJobByRequestID(ctx, "writable")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	require.NoError(t, db.Model(&models.SandboxSpec{}).Where("sandbox_id = ?", sandboxID).UpdateColumn("pending_runtime_attach", "unresolved").Error)
	_, err = SubmitSandboxSnapshot(ctx, "blocked", sandboxID, "host", "127.0.0.1", "blocked", "", snapshotmode.FsOnly)
	require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
	_, err = getTemplateImageJobByRequestID(ctx, "blocked")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

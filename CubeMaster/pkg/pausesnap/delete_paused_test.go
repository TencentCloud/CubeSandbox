// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pausesnap

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type destroyCall struct {
	addr string
	req  *cubebox.DestroyCubeSandboxRequest
}

type cleanupCall struct {
	addr string
	req  *cubebox.CleanupTemplateRequest
}

func setupPauseDeleteTest(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pause.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.PauseSnapshotRecord{}))

	origDB := getDB()
	origDestroy := destroyOnCubelet
	origCleanup := cleanupTemplateOnCubelet
	origAddr := cubeletAddr
	Init(db)
	cubeletAddr = func(hostIP string) string { return hostIP + ":50051" }
	t.Cleanup(func() {
		Init(origDB)
		destroyOnCubelet = origDestroy
		cleanupTemplateOnCubelet = origCleanup
		cubeletAddr = origAddr
	})
	return db
}

func seedPauseBinding(t *testing.T, db *gorm.DB, sandboxID, snapID, status, nodeIP string) {
	t.Helper()
	require.NoError(t, db.Table(constants.PauseSnapshotTableName).Create(&models.PauseSnapshotRecord{
		SnapshotID:   snapID,
		SandboxID:    sandboxID,
		NodeID:       "node-1",
		NodeIP:       nodeIP,
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Status:       status,
		Backend:      constants.SnapshotBackendXFS,
	}).Error)
}

func mockCubeletOK(t *testing.T) (*[]destroyCall, *[]cleanupCall) {
	t.Helper()
	var (
		mu       sync.Mutex
		destroys []destroyCall
		cleans   []cleanupCall
	)
	destroyOnCubelet = func(_ context.Context, addr string, req *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		mu.Lock()
		destroys = append(destroys, destroyCall{addr: addr, req: req})
		mu.Unlock()
		return &cubebox.DestroyCubeSandboxResponse{
			Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
		}, nil
	}
	cleanupTemplateOnCubelet = func(_ context.Context, addr string, req *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		mu.Lock()
		cleans = append(cleans, cleanupCall{addr: addr, req: req})
		mu.Unlock()
		return &cubebox.CleanupTemplateResponse{
			Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
		}, nil
	}
	return &destroys, &cleans
}

func TestBeginCompletePersistsBackendAndRemoteStatus(t *testing.T) {
	setupPauseDeleteTest(t)
	ctx := context.Background()

	snapID, err := Begin(ctx, "sb-s3", "node-1", "10.0.0.9", "cubebox", "s3")
	require.NoError(t, err)
	require.NotEmpty(t, snapID)

	creating, err := GetBySandbox(ctx, "sb-s3")
	require.NoError(t, err)
	require.Equal(t, statusCreating, creating.Status)
	require.Equal(t, constants.SnapshotBackendS3, creating.Backend)
	require.Equal(t, constants.RemoteStatusPending, creating.RemoteStatus)
	require.Equal(t, "10.0.0.9", creating.NodeIP)

	require.NoError(t, Complete(ctx, "sb-s3", snapID, "node-1", "10.0.0.9", "cubebox", []string{"vol-a", "vol-a"}, `{"rootfs":"export-a"}`))
	ready, err := GetBySnapshotID(ctx, snapID)
	require.NoError(t, err)
	require.Equal(t, statusReady, ready.Status)
	require.Equal(t, []string{"vol-a"}, ready.PluginVolumeIDs)
	require.Equal(t, constants.SnapshotBackendS3, ready.Backend)
	require.Equal(t, `{"rootfs":"export-a"}`, ready.ExportUUIDs)
	require.Equal(t, constants.RemoteStatusInProgress, ready.RemoteStatus)

	snapID2, err := Begin(ctx, "sb-s3-noexport", "node-1", "10.0.0.9", "cubebox", "s3")
	require.NoError(t, err)
	require.NoError(t, Complete(ctx, "sb-s3-noexport", snapID2, "node-1", "10.0.0.9", "cubebox", nil, ""))
	ready2, err := GetBySnapshotID(ctx, snapID2)
	require.NoError(t, err)
	require.Equal(t, statusReady, ready2.Status)
	require.Equal(t, constants.RemoteStatusFailed, ready2.RemoteStatus)
	require.Empty(t, ready2.ExportUUIDs)

	_, err = Begin(ctx, "sb-bad", "node-1", "10.0.0.9", "cubebox", "nfs")
	require.Error(t, err)
}

func TestTryDeletePausedNoBinding(t *testing.T) {
	setupPauseDeleteTest(t)
	destroys, cleans := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req", "sb-missing", "10.0.0.1")
	require.NoError(t, err)
	require.False(t, handled)
	require.Empty(t, *destroys)
	require.Empty(t, *cleans)
}

func TestTryDeletePausedReadyUsesTombstone(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-ready", "snap-ready000000000000000001", statusReady, "10.0.0.8")
	destroys, cleans := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req-ready", "sb-ready", "10.0.0.1")
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, *destroys, 1)
	require.Equal(t, "10.0.0.8:50051", (*destroys)[0].addr, "READY replica NodeIP must win over caller hostIP")
	require.Equal(t, "true", (*destroys)[0].req.Annotations[constants.CubeAnnotationPauseDeleteTombstone])
	require.Len(t, *cleans, 1)
	require.Equal(t, "snap-ready000000000000000001", (*cleans)[0].req.TemplateID)
	require.Equal(t, constants.SnapshotBackendXFS, (*cleans)[0].req.Backend)

	_, err = GetBySandbox(context.Background(), "sb-ready")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTryDeletePausedFailedKillsLiveShim(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-failed", "snap-failed00000000000000001", statusFailed, "")
	destroys, cleans := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req-failed", "sb-failed", "10.0.0.2")
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, *destroys, 1)
	require.Equal(t, "10.0.0.2:50051", (*destroys)[0].addr)
	_, hasTombstone := (*destroys)[0].req.Annotations[constants.CubeAnnotationPauseDeleteTombstone]
	require.False(t, hasTombstone, "FAILED pause still has a live shim; must not use tombstone delete")
	require.Len(t, *cleans, 1)
	require.Equal(t, "snap-failed00000000000000001", (*cleans)[0].req.TemplateID)
}

func TestTryDeletePausedCreatingKillsLiveShim(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-creating", "snap-creating000000000000001", statusCreating, "")
	destroys, _ := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req-creating", "sb-creating", "10.0.0.3")
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, *destroys, 1)
	_, hasTombstone := (*destroys)[0].req.Annotations[constants.CubeAnnotationPauseDeleteTombstone]
	require.False(t, hasTombstone, "in-flight Pause still has a live shim")
}

func TestTryDeletePausedEmptyHostIPWithoutReplica(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-noip", "snap-noip00000000000000000001", statusFailed, "")
	destroys, cleans := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req", "sb-noip", "")
	require.Error(t, err)
	require.True(t, handled)
	require.Empty(t, *destroys)
	require.Empty(t, *cleans)
}

func TestTryDeletePausedDestroyNotFoundStillCleans(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-gone", "snap-gone00000000000000000001", statusFailed, "")
	cleanupCalled := false
	destroyOnCubelet = func(_ context.Context, _ string, _ *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_Unknown,
				RetMsg:  "sandbox not found",
			},
		}, nil
	}
	cleanupTemplateOnCubelet = func(_ context.Context, _ string, req *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		cleanupCalled = true
		require.Equal(t, "snap-gone00000000000000000001", req.TemplateID)
		return &cubebox.CleanupTemplateResponse{
			Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
		}, nil
	}

	handled, err := TryDeletePaused(context.Background(), "req", "sb-gone", "10.0.0.4")
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, cleanupCalled)
}

func TestTryDeletePausedDestroyErrorSkipsCleanup(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-busy", "snap-busy00000000000000000001", statusFailed, "")
	cleanupCalled := false
	destroyOnCubelet = func(_ context.Context, _ string, _ *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{
			Ret: &cubeleterrorcode.Ret{
				RetCode: cubeleterrorcode.ErrorCode_TaskStateInvalid,
				RetMsg:  "sandbox lifecycle operation is in progress",
			},
		}, nil
	}
	cleanupTemplateOnCubelet = func(_ context.Context, _ string, _ *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		cleanupCalled = true
		return nil, nil
	}

	handled, err := TryDeletePaused(context.Background(), "req", "sb-busy", "10.0.0.5")
	require.Error(t, err)
	require.True(t, handled)
	require.False(t, cleanupCalled, "must not drop snap while live Destroy failed")
	rec, getErr := GetBySandbox(context.Background(), "sb-busy")
	require.NoError(t, getErr)
	require.Equal(t, "snap-busy00000000000000000001", rec.SnapshotID)
}

func TestTryDeletePausedCleanupErrorKeepsBinding(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-cleanup", "snap-cleanup0000000000000001", statusFailed, "")
	destroyOnCubelet = func(_ context.Context, _ string, _ *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{
			Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
		}, nil
	}
	cleanupTemplateOnCubelet = func(_ context.Context, _ string, _ *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		return nil, errors.New("cubelet unreachable")
	}

	handled, err := TryDeletePaused(context.Background(), "req", "sb-cleanup", "10.0.0.6")
	require.Error(t, err)
	require.True(t, handled)
	rec, getErr := GetBySandbox(context.Background(), "sb-cleanup")
	require.NoError(t, getErr)
	require.Equal(t, "snap-cleanup0000000000000001", rec.SnapshotID)
	require.Equal(t, statusDeleteFailed, rec.Status)
	require.Contains(t, rec.LastError, "cubelet unreachable")
}

func TestTryDeletePausedRetriesDeleteFailedWithTombstone(t *testing.T) {
	db := setupPauseDeleteTest(t)
	seedPauseBinding(t, db, "sb-retry", "snap-retry0000000000000000001", statusDeleteFailed, "10.0.0.7")
	destroys, cleans := mockCubeletOK(t)

	handled, err := TryDeletePaused(context.Background(), "req-retry", "sb-retry", "10.0.0.1")
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, *destroys, 1)
	require.Equal(t, "true", (*destroys)[0].req.Annotations[constants.CubeAnnotationPauseDeleteTombstone],
		"a delete that already reaped the shim must not try to kill it again")
	require.Len(t, *cleans, 1)
	require.Equal(t, "snap-retry0000000000000000001", (*cleans)[0].req.TemplateID)

	_, err = GetBySandbox(context.Background(), "sb-retry")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCheckDestroyRet(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkDestroyRet(nil, "sb", "destroy"))
	require.NoError(t, checkDestroyRet(&cubebox.DestroyCubeSandboxResponse{}, "sb", "destroy"))
	require.NoError(t, checkDestroyRet(&cubebox.DestroyCubeSandboxResponse{
		Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Success},
	}, "sb", "destroy"))
	require.NoError(t, checkDestroyRet(&cubebox.DestroyCubeSandboxResponse{
		Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Unknown, RetMsg: "Not Found"},
	}, "sb", "destroy"))
	require.Error(t, checkDestroyRet(&cubebox.DestroyCubeSandboxResponse{
		Ret: &cubeleterrorcode.Ret{RetCode: cubeleterrorcode.ErrorCode_Unknown, RetMsg: "disk full"},
	}, "sb", "destroy"))
}

func TestDropOriginTombstoneCleansPausePackage(t *testing.T) {
	setupPauseDeleteTest(t)
	destroys, cleans := mockCubeletOK(t)

	err := DropOriginTombstone(context.Background(), "req-x", "sb-x", "10.0.0.8",
		"snap-x000000000000000000000001", constants.SnapshotBackendS3)
	require.NoError(t, err)
	require.Len(t, *destroys, 1)
	require.Equal(t, "10.0.0.8:50051", (*destroys)[0].addr)
	require.Equal(t, "true", (*destroys)[0].req.Annotations[constants.CubeAnnotationPauseDeleteTombstone])
	require.Len(t, *cleans, 1)
	require.Equal(t, "10.0.0.8:50051", (*cleans)[0].addr)
	require.Equal(t, "snap-x000000000000000000000001", (*cleans)[0].req.TemplateID)
	require.Equal(t, constants.SnapshotBackendS3, (*cleans)[0].req.Backend)
}

func TestCleanupPauseSnapshot(t *testing.T) {
	setupPauseDeleteTest(t)
	destroys, cleans := mockCubeletOK(t)

	require.NoError(t, CleanupPauseSnapshot(context.Background(), "req-s", "10.0.0.8",
		"snap-s000000000000000000000001", constants.SnapshotBackendXFS))
	require.Empty(t, *destroys)
	require.Len(t, *cleans, 1)
	require.Equal(t, "10.0.0.8:50051", (*cleans)[0].addr)
	require.Equal(t, "snap-s000000000000000000000001", (*cleans)[0].req.TemplateID)
	require.Equal(t, constants.SnapshotBackendXFS, (*cleans)[0].req.Backend)

	require.NoError(t, CleanupPauseSnapshot(context.Background(), "req-s", "10.0.0.8", "", constants.SnapshotBackendXFS))
	require.NoError(t, CleanupPauseSnapshot(context.Background(), "req-s", "", "snap-s000000000000000000000001", constants.SnapshotBackendXFS))
	require.Len(t, *cleans, 1)
}

func TestDropOriginTombstoneSkipsCleanupWithoutSnapshotID(t *testing.T) {
	setupPauseDeleteTest(t)
	destroys, cleans := mockCubeletOK(t)

	err := DropOriginTombstone(context.Background(), "req-x", "sb-x", "10.0.0.8", "", "")
	require.NoError(t, err)
	require.Len(t, *destroys, 1)
	require.Empty(t, *cleans)
}

func TestDropOriginTombstoneJoinsTombstoneAndCleanupErrors(t *testing.T) {
	setupPauseDeleteTest(t)
	destroyOnCubelet = func(_ context.Context, _ string, _ *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return nil, errors.New("tombstone rpc failed")
	}
	cleanupTemplateOnCubelet = func(_ context.Context, _ string, _ *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		return nil, errors.New("cleanup rpc failed")
	}

	err := DropOriginTombstone(context.Background(), "req-x", "sb-x", "10.0.0.8",
		"snap-x000000000000000000000001", constants.SnapshotBackendS3)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tombstone rpc failed")
	require.Contains(t, err.Error(), "cleanup rpc failed")
}

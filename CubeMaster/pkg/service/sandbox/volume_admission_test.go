// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	codes "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"gorm.io/gorm"
)

func newGuardedCreateFixture(t *testing.T, patches *gomonkey.Patches) (*gorm.DB, uint, *createSandboxContext) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))
	row := models.VolumeRecord{VolumeID: "shared", Name: "shared", Driver: "s3"}
	require.NoError(t, db.Create(&row).Error)
	patches.ApplyFunc(dao.Default, func() *gorm.DB { return db })
	patches.ApplyFunc(localcache.IncrNodeConcurrent, func(*node.Node) error { return nil })
	patches.ApplyFunc(localcache.DecrNodeConcurrent, func(*node.Node) error { return nil })
	c := &createSandboxContext{
		ctx:        t.Context(),
		directHost: true,
		selectHost: &node.Node{IP: "127.0.0.1"},
		masterRsp:  &types.CreateCubeSandboxRes{RequestID: "req-create", Ret: &types.Ret{}},
		cubeletReq: &cubebox.RunCubeSandboxRequest{
			RequestID: "req-create", InstanceType: "cubebox",
			Annotations: map[string]string{
				AnnotationPluginVolumeSources:            `[{"name":"shared","driver":"s3"}]`,
				constants.CubeAnnotationDesiredSandboxID: "sb",
			},
		},
	}
	return db, row.ID, c
}

func TestGuardedCreateFailoverRequiresConfirmedSandbox(t *testing.T) {
	for _, tc := range []struct {
		name, requestID, sandboxID string
		code                       codes.ErrorCode
		ext                        map[string][]byte
		rpcErr                     error
		wantCode                   int
		wantCleanup                bool
	}{
		{"settlement failed", "req-create", "sb", codes.ErrorCode_Success,
			map[string][]byte{refcount.ExtInfoKey: []byte("{")}, nil, int(errorcode.ErrorCode_DBError), true},
		{"report failed", "req-create", "sb", codes.ErrorCode_Unknown,
			map[string][]byte{cubebox.ExtVolumeCreateReportFailed: []byte("true")}, nil, int(codes.ErrorCode_Unknown), true},
		{"already exists", "req-create", "sb", codes.ErrorCode_PreConditionFailed,
			nil, nil, int(codes.ErrorCode_PreConditionFailed), false},
		{"different request", "other", "sb", codes.ErrorCode_Success,
			nil, nil, int(codes.ErrorCode_Unknown), false},
		{"different sandbox", "req-create", "different", codes.ErrorCode_Success,
			nil, nil, int(codes.ErrorCode_Unknown), false},
		{"empty sandbox", "req-create", "", codes.ErrorCode_Success,
			nil, nil, int(codes.ErrorCode_Unknown), false},
		{"response with transport error", "req-create", "sb", codes.ErrorCode_Success,
			nil, errors.New("transport failed"), int(errorcode.ErrorCode_ReqCubeAPIFailed), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			db, id, c := newGuardedCreateFixture(t, patches)
			ctx, cancel := context.WithCancel(c.ctx)
			defer cancel()
			c.ctx = ctx
			endpoint := cubelet.GetCubeletAddr(c.selectHost.HostIP())
			patches.ApplyFunc(cubelet.CreatePrepared, func(_ context.Context, ep string, _ *cubebox.RunCubeSandboxRequest, admit func() error) (*cubebox.RunCubeSandboxResponse, error) {
				require.Equal(t, endpoint, ep)
				require.NoError(t, admit())
				return &cubebox.RunCubeSandboxResponse{
					RequestID: tc.requestID, SandboxID: tc.sandboxID,
					Ret: &codes.Ret{RetCode: tc.code}, ExtInfo: tc.ext,
				}, tc.rpcErr
			})
			var queued *task.Task
			patches.ApplyFunc(task.AddAsyncTask, func(t *task.Task) error { queued = t; return nil })

			require.False(t, c.callCubelet())
			require.True(t, c.volumeAdmissionFailed)
			require.Equal(t, tc.wantCode, c.masterRsp.Ret.RetCode)
			require.Empty(t, c.masterRsp.SandboxID)
			cancel()
			c.selectHost = &node.Node{IP: "127.0.0.2"}
			c.cubeletReq.RequestID = "other"
			c.failover()
			if tc.wantCleanup {
				require.NotNil(t, queued)
				require.NoError(t, queued.Ctx.Err())
				require.Equal(t, task.DestroySandbox, queued.TaskType)
				require.Equal(t, endpoint, queued.CallEp)
				require.True(t, queued.CreateFailover)
				req := queued.Request.(*cubebox.DestroyCubeSandboxRequest)
				require.Equal(t, "req-create", req.GetRequestID())
				require.Equal(t, "sb", req.GetSandboxID())
				require.Equal(t, "cubebox", req.GetFilter().GetInstanceType())
			} else {
				require.Nil(t, queued)
			}
			_, err := admission.BeginDelete(t.Context(), db, id)
			require.ErrorIs(t, err, admission.ErrBusy)
		})
	}
}

func TestGuardedCreateRetriesOnlySettledWorkflowLimit(t *testing.T) {
	cfg := &config.Config{CubeletConf: &config.CubeletConf{LoopMaxRetries: 1}}
	errorcode.InitCubeCodeRetryMap(cfg)
	old := l
	t.Cleanup(func() { l = old })
	require.NoError(t, Init(t.Context(), cfg))

	for _, tc := range []struct {
		name, marker string
		failFinish   bool
		wantCalls    int
		wantPending  int64
		wantCode     int
	}{
		{"limiter before attach", cubebox.ExtVolumeAttachNotStarted, false, 2, 0, int(codes.ErrorCode_PreConditionFailed)},
		{"rolled back", cubebox.ExtVolumeCreateRolledBack, false, 1, 0, int(codes.ErrorCode_ConcurrentFailed)},
		{"settlement failed", cubebox.ExtVolumeAttachNotStarted, true, 1, 1, int(errorcode.ErrorCode_DBError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			db, id, c := newGuardedCreateFixture(t, patches)
			patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) { return c.selectHost, true })
			calls := 0
			if tc.failFinish {
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:fail_settlement", func(tx *gorm.DB) {
					if updates, ok := tx.Statement.Dest.(map[string]any); ok {
						if _, settling := updates["refcount"]; settling {
							tx.AddError(errors.New("settlement failed"))
						}
					}
				}))
			}
			patches.ApplyFunc(cubelet.CreatePrepared, func(ctx context.Context, _ string, req *cubebox.RunCubeSandboxRequest, admit func() error) (*cubebox.RunCubeSandboxResponse, error) {
				calls++
				if calls == 2 {
					var row models.VolumeRecord
					require.NoError(t, db.WithContext(ctx).First(&row, id).Error)
					require.Zero(t, row.InFlightCreates, "settle the first guard before the second attempt")
				}
				require.NoError(t, admit())
				code, marker := codes.ErrorCode_ConcurrentFailed, tc.marker
				if calls > 1 {
					code, marker = codes.ErrorCode_PreConditionFailed, cubebox.ExtVolumeAttachNotStarted
				}
				return &cubebox.RunCubeSandboxResponse{
					RequestID: req.GetRequestID(), Ret: &codes.Ret{RetCode: code},
					ExtInfo: map[string][]byte{marker: []byte("true")},
				}, nil
			})
			c.handleCubelet()
			require.Equal(t, tc.wantCalls, calls)
			require.Equal(t, tc.wantCode, c.masterRsp.Ret.RetCode)
			var row models.VolumeRecord
			require.NoError(t, db.First(&row, id).Error)
			require.Equal(t, tc.wantPending, row.InFlightCreates)
		})
	}
}

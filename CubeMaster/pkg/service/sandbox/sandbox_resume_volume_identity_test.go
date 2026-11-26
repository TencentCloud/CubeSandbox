// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	grpcpool "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/grpc-middleware/pool"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeletcodes "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"gorm.io/gorm"
)

const (
	resumeIdentitySandbox  = "sb-resume-volume"
	resumeIdentitySnapshot = "snap-resume-volume"
)

type resumeVolumeIdentityCubelet struct {
	cubebox.UnimplementedCubeboxMgrServer
	db          *gorm.DB
	reserved    []dbmodels.VolumeRecord
	createCalls atomic.Int32
	listCalls   atomic.Int32
	request     chan *cubebox.RunCubeSandboxRequest
}

func (s *resumeVolumeIdentityCubelet) List(_ context.Context, req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
	s.listCalls.Add(1)
	if req.GetId() != resumeIdentitySandbox {
		return nil, fmt.Errorf("unexpected List sandbox %q", req.GetId())
	}
	return &cubebox.ListCubeSandboxResponse{
		Items: []*cubebox.CubeSandbox{{Id: resumeIdentitySandbox, Containers: []*cubebox.Container{
			{Id: resumeIdentitySandbox, State: cubebox.ContainerState_CONTAINER_PAUSED},
		}}},
	}, nil
}

func (s *resumeVolumeIdentityCubelet) Create(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
	if s.createCalls.Add(1) != 1 {
		return nil, errors.New("duplicate Resume Create")
	}
	s.request <- req
	events := make([]refcount.Event, 0, len(s.reserved))
	for _, expected := range s.reserved {
		var row dbmodels.VolumeRecord
		if err := s.db.WithContext(ctx).Take(&row, expected.ID).Error; err != nil {
			return nil, err
		}
		if row.InFlightCreates != 1 || row.RefCount != 0 || row.Deleting {
			return nil, fmt.Errorf("volume %s was not reserved before Create: %+v", row.VolumeID, row)
		}
		if _, err := admission.BeginDelete(ctx, s.db, row.ID); !errors.Is(err, admission.ErrBusy) {
			return nil, fmt.Errorf("Delete on reserved volume %s returned %v", row.VolumeID, err)
		}
		events = append(events, refcount.Event{VolumeID: row.VolumeID, Referenced: 1})
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	return &cubebox.RunCubeSandboxResponse{
		RequestID: req.GetRequestID(), SandboxID: resumeIdentitySandbox, SandboxIP: "10.0.0.1",
		Ret:     &cubeletcodes.Ret{RetCode: cubeletcodes.ErrorCode_Success},
		ExtInfo: map[string][]byte{refcount.ExtInfoKey: encoded},
	}, nil
}

type resumeVolumeIdentityConn struct{ cc *grpc.ClientConn }

func (c resumeVolumeIdentityConn) Value() *grpc.ClientConn { return c.cc }
func (c resumeVolumeIdentityConn) Close() error            { return nil }

func connectResumeVolumeIdentityCubelet(t *testing.T, s *resumeVolumeIdentityCubelet, patches *gomonkey.Patches) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.WaitForHandlers(true))
	cubebox.RegisterCubeboxMgrServer(server, s)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///resume-volume-identity",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	patches.ApplyFunc(grpcconn.GetWorkerConn, func(context.Context, string) (grpcpool.Conn, error) {
		return resumeVolumeIdentityConn{cc: conn}, nil
	})
}

func TestResumeFromPauseVolumeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, legacy string
		timedOut     bool
		recreated    bool
		corrupt      bool
	}{
		{name: "original volume"},
		{name: "same name replacement", recreated: true},
		{name: "timed out pause with no volume IDs", timedOut: true},
		{name: "timed out pause with a replacement", timedOut: true, recreated: true},
		{name: "old NULL runtime mounts", legacy: "null"},
		{name: "old missing spec", legacy: "missing"},
		{name: "damaged runtime mounts", corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pause-resume.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&dbmodels.PauseSnapshotRecord{}, &dbmodels.SandboxSpec{}, &dbmodels.VolumeRecord{}))
			require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_sandbox_spec_id ON t_cube_sandbox_spec(sandbox_id)").Error)
			require.NoError(t, sandboxspec.Init(db))
			pausesnap.Init(db)
			t.Cleanup(func() { pausesnap.Init(nil) })

			work := dbmodels.VolumeRecord{VolumeID: "a-work", Name: "a-work", Driver: "s3", PrivateData: "work-source"}
			data := dbmodels.VolumeRecord{VolumeID: "z-dataset", Name: "z-dataset", Driver: "s3", PrivateData: "original-data-source"}
			require.NoError(t, db.Create(&work).Error)
			require.NoError(t, db.Create(&data).Error)
			if tc.legacy != "missing" {
				require.NoError(t, sandboxspec.Put(ctx, resumeIdentitySandbox, &types.CreateCubeSandboxReq{
					Volumes: []*types.Volume{{Name: work.VolumeID}},
				}, sandboxspec.PutOptions{}))
				stored, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, resumeIdentitySandbox)
				require.NoError(t, err)
				if tc.legacy == "" {
					require.NoError(t, sandboxspec.RecordRuntimeVolumeMount(ctx, resumeIdentitySandbox, stored.RowID, sandboxspec.RuntimeVolumeMount{
						VolumeID: data.VolumeID, VolumeRecordID: data.ID, ContainerPath: "/data", Readonly: true,
					}))
				}
				if tc.corrupt {
					require.NoError(t, db.Model(&dbmodels.SandboxSpec{}).Where("id = ?", stored.RowID).
						UpdateColumn("runtime_volume_mounts", "{").Error)
				}
			}
			var staticReservationWrites int
			if tc.recreated {
				oldID := data.ID
				require.NoError(t, db.Delete(&data).Error)
				data.ID = 0
				data.PrivateData = "replacement-data-source"
				require.NoError(t, db.Create(&data).Error)
				require.NotEqual(t, oldID, data.ID)
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("observe-first-resume-reservation", func(tx *gorm.DB) {
					if row, ok := tx.Statement.Model.(*dbmodels.VolumeRecord); ok && row.ID == work.ID &&
						tx.Error == nil && tx.RowsAffected == 1 {
						staticReservationWrites++
					}
				}))
			}
			pause := dbmodels.PauseSnapshotRecord{
				SandboxID: resumeIdentitySandbox, SnapshotID: resumeIdentitySnapshot, NodeID: "node",
				NodeIP: "127.0.0.1", InstanceType: "cubebox", Backend: "xfs", Status: pausesnap.StatusReady,
			}
			switch {
			case tc.timedOut:
				pause.Status = pausesnap.StatusFailed
				pause.LastError = "rpc error: code = DeadlineExceeded desc = context deadline exceeded"
			case tc.legacy == "missing":
				pause.PluginVolumeIDs = `["a-work"]`
			case tc.legacy == "":
				pause.PluginVolumeIDs = `["z-dataset"]`
			}
			require.NoError(t, db.Create(&pause).Error)

			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			patches.ApplyFunc(dao.Default, func() *gorm.DB { return db })
			patches.ApplyFunc(refreshProxyMapAfterResume, func(context.Context, string, string, *cubebox.RunCubeSandboxResponse) error { return nil })
			patches.ApplyFunc(cubeproxy.InvalidateBackendCache, func(context.Context, string, string) error { return nil })
			oldCleanup := cleanupPausePackageAfterResume
			cleanupPausePackageAfterResume = func(context.Context, string, string, string, string, string) {}
			t.Cleanup(func() { cleanupPausePackageAfterResume = oldCleanup })
			server := &resumeVolumeIdentityCubelet{
				db: db, reserved: []dbmodels.VolumeRecord{work}, request: make(chan *cubebox.RunCubeSandboxRequest, 1),
			}
			if tc.legacy == "" {
				server.reserved = append(server.reserved, data)
			}
			connectResumeVolumeIdentityCubelet(t, server, patches)

			req := &types.UpdateRequest{RequestID: "resume", SandboxID: resumeIdentitySandbox, InstanceType: "cubebox"}
			res := resumeFromPauseSnapshot(ctx, req, pause.NodeIP)
			wantList := int32(0)
			if tc.timedOut {
				wantList = 1
			}
			require.Equal(t, wantList, server.listCalls.Load())
			if tc.recreated || tc.corrupt {
				require.Zero(t, server.createCalls.Load(), "invalid mount identity must prevent Create dispatch")
				if tc.recreated {
					require.Equal(t, int(errorcode.ErrorCode_Conflict), res.Ret.RetCode, res.Ret.RetMsg)
					require.Contains(t, res.Ret.RetMsg, data.VolumeID)
					require.Equal(t, 1, staticReservationWrites, "the static volume is reserved first and its UPDATE must roll back")
				} else {
					require.Equal(t, int(errorcode.ErrorCode_DBError), res.Ret.RetCode, res.Ret.RetMsg)
				}
				binding, err := pausesnap.GetBySandbox(ctx, resumeIdentitySandbox)
				require.NoError(t, err)
				require.Equal(t, pausesnap.StatusReady, binding.Status)
				if tc.timedOut {
					require.Empty(t, binding.PluginVolumeIDs)
				}
			} else {
				require.Equal(t, int(errorcode.ErrorCode_Success), res.Ret.RetCode, res.Ret.RetMsg)
				require.EqualValues(t, 1, server.createCalls.Load())
				select {
				case create := <-server.request:
					require.Equal(t, req.RequestID, create.GetRequestID())
					require.Equal(t, resumeIdentitySnapshot, create.GetAnnotations()[constants.CubeAnnotationPauseSnapshotID])
					require.Equal(t, resumeIdentitySandbox, create.GetAnnotations()[constants.CubeAnnotationDesiredSandboxID])
					require.Empty(t, create.GetVolumes(), "Resume sends a thin Create")
					var sources []volumeSource
					require.NoError(t, json.Unmarshal([]byte(create.GetAnnotations()[AnnotationPluginVolumeSources]), &sources))
					wantSources := make([]volumeSource, 0, len(server.reserved))
					for _, vol := range server.reserved {
						wantSources = append(wantSources, volumeSource{Name: vol.VolumeID, Driver: vol.Driver, PrivateData: vol.PrivateData})
					}
					require.ElementsMatch(t, wantSources, sources)
				default:
					t.Fatal("missing Create request")
				}
				_, err := pausesnap.GetBySandbox(ctx, resumeIdentitySandbox)
				require.ErrorIs(t, err, pausesnap.ErrNotFound)
			}
			for _, vol := range []dbmodels.VolumeRecord{work, data} {
				var row dbmodels.VolumeRecord
				require.NoError(t, db.Take(&row, vol.ID).Error)
				require.Zero(t, row.InFlightCreates)
				require.False(t, row.Deleting)
				wantRefs := 0
				if !tc.recreated && !tc.corrupt && (vol.ID == work.ID || tc.legacy == "") {
					wantRefs = 1
				}
				require.EqualValues(t, wantRefs, row.RefCount)
			}
		})
	}
}

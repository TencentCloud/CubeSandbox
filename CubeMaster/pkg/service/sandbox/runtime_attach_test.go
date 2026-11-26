// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
)

func TestAttachRuntimeVolume(t *testing.T) {
	server := miniredis.RunT(t)
	redis := wrapredis.GetRedisConnPoolWrap("test-runtime-attach", &config.RedisConf{Nodes: server.Addr(), MaxActive: 4, MaxIdle: 2, MaxRetry: 1})
	t.Cleanup(func() { require.NoError(t, redis.RedisConnPool.Close()) })
	patches := gomonkey.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap { return redis })
	t.Cleanup(patches.Reset)

	t.Run("definite outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			outcome     RuntimeAttachOutcome
			initialRefs int64
			report      *refcount.Event
			wantRefs    int64
		}{
			{name: "first holder", outcome: RuntimeAttachPublished, report: &refcount.Event{VolumeID: "dataset", Referenced: 1}, wantRefs: 1},
			{name: "shared holder", outcome: RuntimeAttachPublished, initialRefs: 1, wantRefs: 1},
			{name: "not started", outcome: RuntimeAttachNotStarted},
			{name: "rolled back", outcome: RuntimeAttachRolledBack, initialRefs: 1, report: &refcount.Event{VolumeID: "dataset", Referenced: 0}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db, req, volume := newRuntimeAttachFixture(t, tc.initialRefs)
				var ext map[string][]byte
				if tc.report != nil {
					encoded, err := json.Marshal([]refcount.Event{*tc.report})
					require.NoError(t, err)
					ext = map[string][]byte{refcount.ExtInfoKey: encoded}
				}
				calls := 0
				err := AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, func(ctx context.Context, binding RuntimeAttachBinding, source models.VolumeRecord) (RuntimeAttachReply, error) {
					calls++
					require.Equal(t, RuntimeAttachBinding{RuntimeAttachRequest: req, VolumeRecordID: volume.ID}, binding)
					require.Equal(t, volume.Driver, source.Driver)
					require.Equal(t, volume.PrivateData, source.PrivateData)
					assertRuntimeAttachState(t, db, req, volume.ID, tc.initialRefs, 1, false)
					_, err := admission.BeginDelete(ctx, db, volume.ID)
					require.ErrorIs(t, err, admission.ErrBusy)
					return RuntimeAttachReply{Binding: binding, Outcome: tc.outcome, ExtInfo: ext}, nil
				})
				require.Equal(t, 1, calls)
				published := tc.outcome == RuntimeAttachPublished
				if published {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrRuntimeAttachRejected)
				}
				assertRuntimeAttachState(t, db, req, volume.ID, tc.wantRefs, 0, published)
				var original models.SandboxSpec
				require.NoError(t, db.Where("sandbox_id = ?", req.SandboxID).Take(&original).Error)
				require.NoError(t, db.Unscoped().Delete(&original).Error)
				db = reopenRuntimeAttachTestDB(t, db)
				daoPatch := gomonkey.ApplyFunc(dao.Default, func() *gorm.DB { return db })
				t.Cleanup(daoPatch.Reset)
				redisPatch := gomonkey.ApplyFunc(wrapredis.GetRedis, func() *wrapredis.RedisWrap {
					t.Fatal("replay must not query Redis")
					return nil
				})
				t.Cleanup(redisPatch.Reset)
				err = AttachRuntimeVolume(t.Context(), req, nil, nil)
				if published {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrRuntimeAttachRejected)
				}
				for _, change := range []func(*RuntimeAttachRequest){
					func(r *RuntimeAttachRequest) { r.ExecutionID += "-other" },
					func(r *RuntimeAttachRequest) { r.VolumeID = "work" },
					func(r *RuntimeAttachRequest) { r.ContainerPath = "/other" },
					func(r *RuntimeAttachRequest) { r.Readonly = !r.Readonly },
				} {
					conflict := req
					change(&conflict)
					require.ErrorIs(t, AttachRuntimeVolume(t.Context(), conflict, nil, nil), ErrRuntimeAttachConflict)
				}
				require.NoError(t, db.Take(&volume, volume.ID).Error)
				require.Equal(t, tc.wantRefs, volume.RefCount)
				require.Zero(t, volume.InFlightCreates)
			})
		}
	})

	t.Run("legacy marker still blocks without a binding", func(t *testing.T) {
		db, req, volume := newRuntimeAttachFixture(t, 0)
		require.NoError(t, db.Model(&models.SandboxSpec{}).Where("sandbox_id = ?", req.SandboxID).
			UpdateColumn("pending_runtime_attach", "87e39dd4-a0cd-48ce-b1d9-84f10e4543c4").Error)
		require.NoError(t, db.Model(&volume).UpdateColumn("in_flight_creates", 1).Error)
		require.ErrorIs(t, AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, unexpectedRuntimeAttachDispatch), sandboxspec.ErrRuntimeAttachPending)
		assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)
		_, err := admission.BeginDelete(t.Context(), db, volume.ID)
		require.ErrorIs(t, err, admission.ErrBusy)
	})

	t.Run("admission request write failure rolls back", func(t *testing.T) {
		db, req, volume := newRuntimeAttachFixture(t, 0)
		require.NoError(t, db.Exec(`CREATE TRIGGER reject_attach_request BEFORE INSERT ON t_cube_runtime_volume_attach
			BEGIN SELECT RAISE(ABORT, 'attach request insert failed'); END`).Error)
		require.ErrorContains(t, AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, unexpectedRuntimeAttachDispatch), "attach request insert failed")
		assertRuntimeAttachState(t, db, req, volume.ID, 0, 0, false)
	})

	t.Run("trusted terminal retry after a rolled back write", func(t *testing.T) {
		db, req, volume := newRuntimeAttachFixture(t, 0)
		require.NoError(t, db.Exec(`CREATE TRIGGER reject_attach_outcome BEFORE UPDATE OF outcome ON t_cube_runtime_volume_attach
			BEGIN SELECT RAISE(ABORT, 'attach outcome write failed'); END`).Error)
		reply := RuntimeAttachReply{Binding: RuntimeAttachBinding{RuntimeAttachRequest: req, VolumeRecordID: volume.ID},
			Outcome: RuntimeAttachPublished, ExtInfo: runtimeAttachFirstReference(t, req.VolumeID)}
		err := AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, func(context.Context, RuntimeAttachBinding, models.VolumeRecord) (RuntimeAttachReply, error) {
			return reply, nil
		})
		require.ErrorIs(t, err, ErrRuntimeAttachUnresolved)
		require.ErrorContains(t, err, "attach outcome write failed")
		assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)
		require.ErrorIs(t, AttachRuntimeVolume(t.Context(), req, nil, nil), ErrRuntimeAttachUnresolved)

		db = reopenRuntimeAttachTestDB(t, db)
		require.NoError(t, sandboxspec.Init(db))
		require.NoError(t, db.Exec("DROP TRIGGER reject_attach_outcome").Error)
		badReply := reply
		badReply.Binding.VolumeRecordID++
		require.ErrorIs(t, settleRuntimeAttach(t.Context(), db, req, badReply), ErrRuntimeAttachUnresolved)
		require.NoError(t, settleRuntimeAttach(t.Context(), db, req, reply))
		require.NoError(t, settleRuntimeAttach(t.Context(), db, req, reply))
		assertRuntimeAttachState(t, db, req, volume.ID, 1, 0, true)
	})

	t.Run("unknown does not release protection", func(t *testing.T) {
		for _, scenario := range []string{"transport", "wrong execution", "wrong request", "wrong path", "wrong volume row", "unknown status", "contradictory not started", "settlement failure"} {
			t.Run(scenario, func(t *testing.T) {
				db, req, volume := newRuntimeAttachFixture(t, 0)
				if scenario == "settlement failure" {
					require.NoError(t, db.Exec(`CREATE TRIGGER reject_mount BEFORE UPDATE OF runtime_volume_mounts ON t_cube_sandbox_spec
						BEGIN SELECT RAISE(ABORT, 'mount write failed'); END`).Error)
				}
				err := AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, func(_ context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
					reply := RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachPublished, ExtInfo: runtimeAttachFirstReference(t, req.VolumeID)}
					switch scenario {
					case "transport":
						return reply, errors.New("reply was not delivered")
					case "wrong execution":
						reply.Binding.ExecutionID += "-replaced"
					case "wrong request":
						reply.Binding.RequestID += "-other"
					case "wrong path":
						reply.Binding.ContainerPath = "/other"
					case "wrong volume row":
						reply.Binding.VolumeRecordID++
					case "unknown status":
						reply.Outcome = RuntimeAttachUnknown
					case "contradictory not started":
						reply.Outcome = RuntimeAttachNotStarted
					}
					return reply, nil
				})
				require.ErrorIs(t, err, ErrRuntimeAttachUnresolved)
				if scenario == "settlement failure" {
					require.ErrorContains(t, err, "mount write failed")
				}
				assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)
				_, err = admission.BeginDelete(t.Context(), db, volume.ID)
				require.ErrorIs(t, err, admission.ErrBusy)
				require.ErrorIs(t, AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, unexpectedRuntimeAttachDispatch), ErrRuntimeAttachUnresolved)
			})
		}
	})

	t.Run("blocks conflicting admission before dispatch", func(t *testing.T) {
		for _, scenario := range []string{"pause creating", "pause ready", "pause timed out", "snapshot active", "existing parent mount", "existing child mount", "annotation mount", "duplicate volume"} {
			t.Run(scenario, func(t *testing.T) {
				db, req, volume := newRuntimeAttachFixture(t, 0)
				checkSnapshot := noActiveRuntimeAttachSnapshot
				var want string
				switch scenario {
				case "pause creating", "pause ready", "pause timed out":
					status := map[string]string{"pause creating": pausesnap.StatusCreating, "pause ready": pausesnap.StatusReady, "pause timed out": pausesnap.StatusFailed}[scenario]
					require.NoError(t, db.Create(&models.PauseSnapshotRecord{SandboxID: req.SandboxID, SnapshotID: "snap-active", Status: status, LastError: "DeadlineExceeded"}).Error)
					want = "already has pause snapshot"
				case "snapshot active":
					checkSnapshot = func(tx *gorm.DB, sandboxID string) error {
						require.Equal(t, req.SandboxID, sandboxID)
						require.True(t, server.Exists(rediskey.LockSandbox(sandboxID)))
						var spec models.SandboxSpec
						require.NoError(t, tx.Where("sandbox_id = ?", sandboxID).Take(&spec).Error)
						require.NotNil(t, spec.PendingRuntimeAttach)
						var reserved models.VolumeRecord
						require.NoError(t, tx.Take(&reserved, volume.ID).Error)
						require.EqualValues(t, 1, reserved.InFlightCreates)
						return errors.New("active snapshot job")
					}
					want = "active snapshot job"
				case "existing parent mount":
					req.ContainerPath = "/work/nested"
					want = "conflicts with existing mount"
				case "existing child mount":
					req.ContainerPath = "/work"
					base := &types.CreateCubeSandboxReq{Containers: []*types.Container{{
						VolumeMounts: []*cubebox.VolumeMounts{{Name: "work", ContainerPath: "/work/nested"}},
					}}}
					require.NoError(t, sandboxspec.Put(t.Context(), req.SandboxID, base, sandboxspec.PutOptions{}))
					want = "conflicts with existing mount"
				case "annotation mount":
					req.ContainerPath = "/work/nested"
					base := &types.CreateCubeSandboxReq{
						Volumes:    []*types.Volume{{Name: "work"}},
						Containers: []*types.Container{{Name: "main"}},
						Annotations: map[string]string{
							AnnotationPluginVolumeMounts: `[{"name":"work","container_path":"/work","readonly":true}]`,
						},
					}
					require.NoError(t, sandboxspec.Put(t.Context(), req.SandboxID, base, sandboxspec.PutOptions{}))
					want = "conflicts with existing mount"
				case "duplicate volume":
					req.VolumeID = "work"
					want = "already declared"
				}
				require.ErrorContains(t, AttachRuntimeVolume(t.Context(), req, checkSnapshot, unexpectedRuntimeAttachDispatch), want)
				assertRuntimeAttachState(t, db, req, volume.ID, 0, 0, false)
			})
		}
	})

	t.Run("rechecks a stale target after lock expiry", func(t *testing.T) {
		db, req, volume := newRuntimeAttachFixture(t, 0)
		other := models.VolumeRecord{VolumeID: "other", Name: "other", Driver: "s3"}
		require.NoError(t, db.Create(&other).Error)
		read, released, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer func() {
			close(released)
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("stale attach worker did not exit")
			}
		}()
		resumeRead := make(chan struct{}, 1)
		blockFirstRead := make(chan struct{}, 1)
		blockFirstRead <- struct{}{}
		require.NoError(t, db.Callback().Query().After("gorm:query").Register("hold-stale-attach-spec-read", func(query *gorm.DB) {
			spec, ok := query.Statement.Dest.(*models.SandboxSpec)
			if !ok || spec.ID == 0 || query.Error != nil {
				return
			}
			select {
			case <-blockFirstRead:
			default:
				return
			}
			close(read)
			select {
			case <-released:
			case <-resumeRead:
			case <-query.Statement.Context.Done():
				_ = query.AddError(query.Statement.Context.Err())
			}
		}))
		firstResult := make(chan error, 1)
		go func() {
			defer close(finished)
			firstResult <- AttachRuntimeVolume(t.Context(), req, noActiveRuntimeAttachSnapshot, unexpectedRuntimeAttachDispatch)
		}()
		select {
		case <-read:
		case err := <-firstResult:
			t.Fatalf("attach returned before reading its spec: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("attach did not read its spec")
		}
		server.FastForward(2 * sandboxlock.LifecycleTTL)
		winningReq := req
		winningReq.RequestID, winningReq.VolumeID = "winning-request", other.VolumeID
		require.NoError(t, AttachRuntimeVolume(t.Context(), winningReq, noActiveRuntimeAttachSnapshot, func(_ context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
			return RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachPublished, ExtInfo: runtimeAttachFirstReference(t, other.VolumeID)}, nil
		}))
		resumeRead <- struct{}{}
		select {
		case err := <-firstResult:
			require.ErrorContains(t, err, "conflicts with existing mount")
		case <-time.After(5 * time.Second):
			t.Fatal("stale attach did not finish")
		}
		assertRuntimeAttachState(t, db, winningReq, other.ID, 1, 0, true)
		require.NoError(t, db.Take(&volume, volume.ID).Error)
		require.Zero(t, volume.RefCount)
		require.Zero(t, volume.InFlightCreates)
	})

	t.Run("late original response replays the other settlement", func(t *testing.T) {
		db, req, volume := newRuntimeAttachFixture(t, 0)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		entered := make(chan RuntimeAttachReply, 1)
		release := make(chan struct{}, 1)
		done := make(chan error, 1)
		finished := make(chan struct{})
		defer func() {
			select {
			case release <- struct{}{}:
			default:
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Error("late attach worker did not exit")
			}
		}()
		ext := runtimeAttachFirstReference(t, req.VolumeID)
		go func() {
			defer close(finished)
			done <- AttachRuntimeVolume(ctx, req, noActiveRuntimeAttachSnapshot, func(workCtx context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
				reply := RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachPublished, ExtInfo: ext}
				entered <- reply
				select {
				case <-release:
					return reply, nil
				case <-workCtx.Done():
					return RuntimeAttachReply{}, workCtx.Err()
				}
			})
		}()
		var reply RuntimeAttachReply
		select {
		case reply = <-entered:
		case <-ctx.Done():
			t.Fatal("attach did not enter dispatch")
		}
		other, _, err := admission.BeginCreate(ctx, db, []string{req.VolumeID})
		require.NoError(t, err)
		server.FastForward(2 * sandboxlock.LifecycleTTL)
		require.ErrorIs(t, AttachRuntimeVolume(ctx, req, nil, nil), ErrRuntimeAttachUnresolved)
		require.NoError(t, settleRuntimeAttach(ctx, db, req, reply))
		release <- struct{}{}
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("late attach did not return")
		}
		require.NoError(t, db.Take(&volume, volume.ID).Error)
		require.EqualValues(t, 1, volume.InFlightCreates)
		require.EqualValues(t, 1, volume.RefCount)
		require.NoError(t, other.Finish(ctx, nil, false))
	})

	t.Run("pause waits through cancellation and lock expiry", func(t *testing.T) {
		for _, publish := range []bool{true, false} {
			name := "confirmed reply"
			if !publish {
				name = "lost reply"
			}
			t.Run(name, func(t *testing.T) {
				db, req, volume := newRuntimeAttachFixture(t, 0)
				localcache.SetSandboxCache(req.SandboxID, &localcache.SandboxCache{SandboxID: req.SandboxID, HostIP: "127.0.0.1"})
				t.Cleanup(func() { localcache.DeleteSandboxCache(req.SandboxID) })
				pauseDispatches := 0
				pausePatch := gomonkey.ApplyFunc(cubelet.UpdateWithTimeout, func(ctx context.Context, _ string, pauseReq *cubebox.UpdateCubeSandboxRequest, _ time.Duration) (*cubebox.UpdateCubeSandboxResponse, error) {
					pauseDispatches++
					require.Equal(t, req.SandboxID, pauseReq.GetSandboxID())
					assertRuntimeAttachState(t, db, req, volume.ID, 1, 0, true)
					return nil, errors.New("stopped at pause dispatch")
				})
				t.Cleanup(pausePatch.Reset)
				pause := func(ctx context.Context) *types.Ret {
					return Update(ctx, &types.UpdateRequest{SandboxID: req.SandboxID, InstanceType: "cubebox", RequestID: "pause", Action: "pause"}).Ret
				}
				clientCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered, release := make(chan struct{}), make(chan struct{}, 1)
				finished := make(chan struct{})
				result := make(chan error, 1)
				t.Cleanup(func() {
					select {
					case release <- struct{}{}:
					default:
					}
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("attach worker did not exit")
					}
				})
				ext := runtimeAttachFirstReference(t, req.VolumeID)
				go func() {
					defer close(finished)
					result <- AttachRuntimeVolume(clientCtx, req, noActiveRuntimeAttachSnapshot, func(ctx context.Context, binding RuntimeAttachBinding, _ models.VolumeRecord) (RuntimeAttachReply, error) {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return RuntimeAttachReply{}, ctx.Err()
						}
						if !publish {
							return RuntimeAttachReply{}, errors.New("reply was not delivered")
						}
						return RuntimeAttachReply{Binding: binding, Outcome: RuntimeAttachPublished, ExtInfo: ext}, nil
					})
				}()
				select {
				case <-entered:
				case err := <-result:
					t.Fatalf("attach returned before dispatch: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("attach did not enter dispatch")
				}
				cancel()
				pauseCtx, stopPause := context.WithTimeout(t.Context(), 50*time.Millisecond)
				ret := pause(pauseCtx)
				stopPause()
				require.EqualValues(t, errorcode.ErrorCode_Conflict, ret.RetCode)
				require.Contains(t, ret.RetMsg, "sandbox lifecycle operation in progress")
				require.Zero(t, pauseDispatches)
				assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)

				server.FastForward(2 * sandboxlock.LifecycleTTL)
				require.False(t, server.Exists(rediskey.LockSandbox(req.SandboxID)))
				ret = pause(t.Context())
				require.EqualValues(t, errorcode.ErrorCode_Conflict, ret.RetCode)
				require.Contains(t, ret.RetMsg, sandboxspec.ErrRuntimeAttachPending.Error())
				require.Zero(t, pauseDispatches)
				_, err := pausesnap.GetBySandbox(t.Context(), req.SandboxID)
				require.ErrorIs(t, err, pausesnap.ErrNotFound)

				release <- struct{}{}
				select {
				case err = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("attach did not finish")
				}
				if publish {
					require.NoError(t, err)
					require.Contains(t, pause(t.Context()).RetMsg, "stopped at pause dispatch")
					require.Equal(t, 1, pauseDispatches)
				} else {
					require.ErrorIs(t, err, ErrRuntimeAttachUnresolved)
					require.Contains(t, pause(t.Context()).RetMsg, sandboxspec.ErrRuntimeAttachPending.Error())
					require.Zero(t, pauseDispatches)
					assertRuntimeAttachState(t, db, req, volume.ID, 0, 1, false)
				}
			})
		}
	})
}

func newRuntimeAttachFixture(t *testing.T, refs int64) (*gorm.DB, RuntimeAttachRequest, models.VolumeRecord) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "runtime-attach.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&models.SandboxSpec{}, &models.VolumeRecord{}, &models.PauseSnapshotRecord{}, &models.RuntimeVolumeAttach{}))
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_sandbox_spec_id ON t_cube_sandbox_spec(sandbox_id)").Error)
	require.NoError(t, sandboxspec.Init(db))
	pausesnap.Init(db)
	t.Cleanup(func() { pausesnap.Init(nil) })
	patches := gomonkey.ApplyFunc(dao.Default, func() *gorm.DB { return db })
	t.Cleanup(patches.Reset)

	req := RuntimeAttachRequest{SandboxID: "sb-test-runtime-attach", RequestID: "attach-request", ExecutionID: "execution-a", VolumeID: "dataset", ContainerPath: "/data", Readonly: true}
	volume := models.VolumeRecord{VolumeID: req.VolumeID, Name: req.VolumeID, RefCount: refs, Driver: "s3", PrivateData: "resolved-data"}
	require.NoError(t, db.Create(&volume).Error)
	require.NoError(t, db.Create(&models.VolumeRecord{VolumeID: "work", Name: "work", RefCount: 1}).Error)
	require.NoError(t, sandboxspec.Put(t.Context(), req.SandboxID, &types.CreateCubeSandboxReq{
		Volumes: []*types.Volume{{Name: "work"}},
		Containers: []*types.Container{{VolumeMounts: []*cubebox.VolumeMounts{
			{Name: "rootfs", ContainerPath: "/"}, {Name: "work", ContainerPath: "/work"},
		}}},
	}, sandboxspec.PutOptions{}))
	return db, req, volume
}

func reopenRuntimeAttachTestDB(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	var databases []struct{ File string }
	require.NoError(t, db.Raw("PRAGMA database_list").Scan(&databases).Error)
	require.NotEmpty(t, databases)
	require.NotEmpty(t, databases[0].File)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	reopened, err := gorm.Open(sqlite.Open(databases[0].File), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err = reopened.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return reopened
}

func assertRuntimeAttachState(t *testing.T, db *gorm.DB, req RuntimeAttachRequest, volumeRowID uint, refs, pending int64, published bool) {
	t.Helper()
	var row models.VolumeRecord
	require.NoError(t, db.Take(&row, volumeRowID).Error)
	require.Equal(t, refs, row.RefCount)
	require.Equal(t, pending, row.InFlightCreates)
	err := sandboxspec.EnsureNoPendingRuntimeAttach(t.Context(), req.SandboxID)
	if pending != 0 {
		require.ErrorIs(t, err, sandboxspec.ErrRuntimeAttachPending)
	} else {
		require.NoError(t, err)
	}
	stored, err := sandboxspec.GetWithRuntimeVolumeMounts(t.Context(), req.SandboxID)
	require.NoError(t, err)
	if published {
		require.Equal(t, []sandboxspec.RuntimeVolumeMount{{
			VolumeID: req.VolumeID, VolumeRecordID: volumeRowID, ContainerPath: req.ContainerPath, Readonly: req.Readonly,
		}}, stored.Mounts)
	} else {
		require.Empty(t, stored.Mounts)
	}
}

func noActiveRuntimeAttachSnapshot(*gorm.DB, string) error { return nil }

func unexpectedRuntimeAttachDispatch(context.Context, RuntimeAttachBinding, models.VolumeRecord) (RuntimeAttachReply, error) {
	return RuntimeAttachReply{}, errors.New("unexpected Attach dispatch")
}

func runtimeAttachFirstReference(t *testing.T, volumeID string) map[string][]byte {
	t.Helper()
	encoded, err := json.Marshal([]refcount.Event{{VolumeID: volumeID, Referenced: 1}})
	require.NoError(t, err)
	return map[string][]byte{refcount.ExtInfoKey: encoded}
}

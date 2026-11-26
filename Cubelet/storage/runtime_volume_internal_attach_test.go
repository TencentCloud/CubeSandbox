// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build runtime_mount_integration

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	masterrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gorm.io/gorm"
)

const (
	internalAttachRequestKey = "runtime-mount-fixture-input"
	internalAttachBindingKey = "runtime-mount-fixture-binding"
	internalAttachTarget     = "/data"
	internalAttachTimeout    = 5 * time.Second
)

type internalAttachBinding struct {
	SandboxID      string
	Namespace      string
	ExecutionStart int64
	VolumeID       string
	Target         string
	ReadOnly       bool
}

type internalAttachInput struct {
	internalAttachBinding
	Driver      string
	PrivateData string
}

// The test server runs the node-side storage path over the existing Update
// transport. Its payload and publication callback are local fixture details.
type internalAttachServer struct {
	cubebox.UnimplementedCubeboxMgrServer
	backend   *runtimeBackendFixture
	sandboxes *cubeboxstore.Store
	locks     *utils.ResourceLocks
	ready     chan struct{}
	release   <-chan struct{}
	finished  chan struct{}
	loseReply bool

	calls        atomic.Int32
	publications int
	reports      int
	published    internalAttachBinding
}

func (s *internalAttachServer) Update(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
	if s.calls.Add(1) != 1 {
		return nil, status.Error(codes.AlreadyExists, "fixture accepts one Update")
	}
	defer close(s.finished)

	var input internalAttachInput
	if err := json.Unmarshal([]byte(req.GetAnnotations()[internalAttachRequestKey]), &input); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid attach input")
	}
	if req.GetRequestID() == "" || req.GetSandboxID() != input.SandboxID ||
		input.Target != internalAttachTarget || !input.ReadOnly {
		return nil, status.Error(codes.InvalidArgument, "expected a read-only /data binding for the requested sandbox")
	}
	unlock, err := s.locks.LockContext(ctx, input.SandboxID)
	if err != nil {
		return nil, status.FromContextError(err).Err()
	}
	defer unlock()
	sandbox, err := s.sandboxes.Get(input.SandboxID)
	if err != nil || sandbox.ID != input.SandboxID || sandbox.Namespace != input.Namespace {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is unavailable or outside the expected running namespace")
	}
	running := sandbox.GetStatus().Get()
	if running.State() != cubebox.ContainerState_CONTAINER_RUNNING || running.StartedAt != input.ExecutionStart {
		return nil, status.Error(codes.FailedPrecondition, "sandbox execution changed")
	}

	var publishErr error
	err = prepareRuntimeVolumeBackendForTest(ctx, s.backend.storage, &volume.AttachRequest{
		SandboxID:     input.SandboxID,
		Namespace:     sandbox.Namespace,
		VolumeID:      input.VolumeID,
		Driver:        input.Driver,
		PrivateData:   input.PrivateData,
		VolumeBaseDir: s.backend.storage.volumePluginBaseDir(),
	}, func() {
		publishErr = s.publish(ctx, input.internalAttachBinding)
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if publishErr != nil {
		return nil, publishErr
	}

	ids, err := volume.Global().TakeCreateReports(sandbox.Namespace, sandbox.ID, []string{input.VolumeID})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.reports = len(ids)
	ext := make(map[string][]byte)
	if len(ids) > 0 {
		events := make([]workflow.VolumeRefEvent, 0, len(ids))
		for _, id := range ids {
			events = append(events, workflow.VolumeRefEvent{VolumeID: id, Referenced: 1})
		}
		ext[constants.CubeExtVolumeRefEvents], err = json.Marshal(events)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	proof, err := json.Marshal(s.published)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if s.loseReply {
		return nil, status.Error(codes.Unavailable, "simulated loss after node publication and reference claim")
	}
	ext[internalAttachBindingKey] = proof
	return &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
		ExtInfo:   ext,
	}, nil
}

func (s *internalAttachServer) publish(ctx context.Context, binding internalAttachBinding) error {
	s.publications++
	stored, err := s.backend.storage.readBackendFileInfo(ctx, binding.SandboxID)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	result := s.backend.driver.results[binding.VolumeID]
	backend := stored.PluginVolumeBackendInfos[binding.VolumeID]
	if stored.Namespace != binding.Namespace || result == nil || backend == nil ||
		backend.VolumeID != binding.VolumeID || backend.Driver != s.backend.driver.Name() ||
		backend.HostPath != result.HostPath || !reflect.DeepEqual(backend.Metadata, result.Metadata) ||
		!reflect.DeepEqual(stored.PluginVolumeBackendInfos[runtimeBackendOldVolume], s.backend.initial.PluginVolumeBackendInfos[runtimeBackendOldVolume]) {
		return status.Error(codes.FailedPrecondition, "missing or mismatched stored backend at publication")
	}

	close(s.ready)
	select {
	case <-s.release:
		s.published = binding
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func attachReadOnlyForTest(ctx context.Context, db *gorm.DB, client cubebox.CubeboxMgrClient, requestID string, binding internalAttachBinding) error {
	spec, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, binding.SandboxID)
	if err != nil {
		return err
	}
	guard, rows, err := admission.BeginCreate(ctx, db, []string{binding.VolumeID})
	if err != nil {
		return err
	}
	row := rows[0]
	binding.VolumeID = row.VolumeID
	input, err := json.Marshal(internalAttachInput{
		internalAttachBinding: binding,
		Driver:                row.Driver,
		PrivateData:           row.PrivateData,
	})
	if err != nil {
		return err
	}
	reply, err := client.Update(ctx, &cubebox.UpdateCubeSandboxRequest{
		RequestID:   requestID,
		SandboxID:   binding.SandboxID,
		Annotations: map[string]string{internalAttachRequestKey: string(input)},
	})
	if err != nil {
		return err
	}
	if reply == nil || reply.GetRet() == nil || reply.GetRet().GetRetCode() != errorcode.ErrorCode_Success ||
		reply.GetRequestID() != requestID {
		return errors.New("missing or mismatched successful attach reply")
	}
	var published internalAttachBinding
	ext := reply.GetExtInfo()
	if err := json.Unmarshal(ext[internalAttachBindingKey], &published); err != nil || published != binding {
		return errors.New("attach reply did not confirm the requested binding")
	}
	return guard.FinishRuntimeAttach(ctx, ext, binding.SandboxID, spec.RowID, sandboxspec.RuntimeVolumeMount{
		VolumeID: row.VolumeID, VolumeRecordID: row.ID, ContainerPath: binding.Target, Readonly: binding.ReadOnly,
	})
}

func addInternalAttachRunningSandbox(sandboxes *cubeboxstore.Store, id, namespace string, startedAt int64) {
	sandbox := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: id}, Namespace: namespace}
	sandbox.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: id},
		Status:   cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: startedAt}),
		IsPod:    true,
	})
	sandboxes.Add(sandbox)
}

func establishInternalAttachNodeReference(t *testing.T, db *gorm.DB, backend *runtimeBackendFixture, volumeID, sandboxID, privateData string) {
	t.Helper()
	ctx := t.Context()
	guard, _, err := admission.BeginCreate(ctx, db, []string{volumeID})
	require.NoError(t, err)
	req := backend.request(volumeID)
	req.SandboxID = sandboxID
	req.PrivateData = privateData
	_, err = volume.Global().Attach(ctx, req)
	require.NoError(t, err)
	reports, err := volume.Global().TakeCreateReports(req.Namespace, req.SandboxID, []string{volumeID})
	require.NoError(t, err)
	require.Equal(t, []string{volumeID}, reports)
	encoded, err := json.Marshal([]workflow.VolumeRefEvent{{VolumeID: volumeID, Referenced: 1}})
	require.NoError(t, err)
	require.NoError(t, guard.Finish(ctx, map[string][]byte{masterrefcount.ExtInfoKey: encoded}, false))
}

func internalAttachClient(t *testing.T, service *internalAttachServer) cubebox.CubeboxMgrClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.WaitForHandlers(true))
	cubebox.RegisterCubeboxMgrServer(server, service)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("stop fixture RPC server: %v", err)
			}
		case <-time.After(internalAttachTimeout):
			t.Error("fixture RPC server did not exit")
		}
	})
	conn, err := grpc.NewClient("passthrough:///runtime-mount-fixture",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDisableRetry(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close fixture RPC client: %v", err)
		}
	})
	return cubebox.NewCubeboxMgrClient(conn)
}

func TestInternalRuntimeAttachReadOnly(t *testing.T) {
	const (
		firstHolderSandbox = "first-holder"
		executionStart     = 123
		mountWriteError    = "fixture rejected mount write after ref settlement"
	)
	for _, tc := range []struct {
		name         string
		sharedHolder bool
		loseReply    bool
		rejectWrite  bool
		deleteSpec   bool
		wantRefs     int64
		wantBusy     int64
	}{
		{name: "settles confirmed read-only attachment", wantRefs: 1},
		{name: "settles shared node attachment without incrementing refs", sharedHolder: true, wantRefs: 1},
		{name: "retains reservation after lost reply", loseReply: true, wantBusy: 1},
		{name: "rolls back reference settlement when mount write fails", rejectWrite: true, wantBusy: 1},
		{name: "rejects response for a deleted spec row", deleteSpec: true, wantBusy: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newRuntimeBackendFixture(t)
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "master.sqlite")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}, &models.SandboxSpec{}))
			require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_sandbox_spec_id ON t_cube_sandbox_spec(sandbox_id)").Error)
			require.NoError(t, sandboxspec.Init(db))
			row := models.VolumeRecord{
				VolumeID:    runtimeBackendNewVolume,
				Name:        runtimeBackendNewVolume,
				Driver:      backend.driver.Name(),
				PrivateData: "opaque:master-volume-row",
			}
			require.NoError(t, db.Create(&row).Error)

			binding := internalAttachBinding{
				SandboxID:      runtimeBackendSandboxID,
				Namespace:      runtimeBackendNamespace,
				ExecutionStart: executionStart,
				VolumeID:       row.VolumeID,
				Target:         internalAttachTarget,
				ReadOnly:       true,
			}
			sandboxes := cubeboxstore.NewStore(nil)
			addInternalAttachRunningSandbox(sandboxes, binding.SandboxID, binding.Namespace, binding.ExecutionStart)
			var initialRefs int64
			if tc.sharedHolder {
				addInternalAttachRunningSandbox(sandboxes, firstHolderSandbox, binding.Namespace, executionStart+1)
				establishInternalAttachNodeReference(t, db, backend, row.VolumeID, firstHolderSandbox, row.PrivateData)
				initialRefs = 1
			}

			base := &sandboxtypes.CreateCubeSandboxReq{Labels: map[string]string{"fixture": "keep-base-spec"}}
			require.NoError(t, sandboxspec.Put(t.Context(), binding.SandboxID, base, sandboxspec.PutOptions{}))
			specBefore, err := sandboxspec.GetWithRuntimeVolumeMounts(t.Context(), binding.SandboxID)
			require.NoError(t, err)
			previousVolume := models.VolumeRecord{VolumeID: runtimeBackendOldVolume, Name: runtimeBackendOldVolume, Driver: row.Driver}
			require.NoError(t, db.Create(&previousVolume).Error)
			previousMount := sandboxspec.RuntimeVolumeMount{
				VolumeID: previousVolume.VolumeID, VolumeRecordID: previousVolume.ID, ContainerPath: "/work", Readonly: true,
			}
			require.NoError(t, sandboxspec.RecordRuntimeVolumeMount(t.Context(), binding.SandboxID, specBefore.RowID, previousMount))
			var specRowBefore models.SandboxSpec
			require.NoError(t, db.Take(&specRowBefore, specBefore.RowID).Error)

			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			service := &internalAttachServer{
				backend: backend, sandboxes: sandboxes, locks: utils.NewResourceLocks(),
				ready: make(chan struct{}), release: release, finished: make(chan struct{}),
				loseReply: tc.loseReply,
			}
			client := internalAttachClient(t, service)
			ctx, cancel := context.WithTimeout(t.Context(), internalAttachTimeout)
			attachDone := make(chan struct{})
			var attachErr error
			t.Cleanup(func() {
				unblock()
				cancel()
				select {
				case <-attachDone:
				case <-time.After(internalAttachTimeout):
					t.Error("fixture attach client did not exit")
				}
			})
			go func() {
				defer close(attachDone)
				attachErr = attachReadOnlyForTest(ctx, db, client, "local-attach-request", binding)
			}()

			requireProtected := func(refs, inFlight int64) {
				t.Helper()
				var current models.VolumeRecord
				require.NoError(t, db.WithContext(ctx).Take(&current, row.ID).Error)
				require.Equal(t, refs, current.RefCount)
				require.Equal(t, inFlight, current.InFlightCreates)
				require.False(t, current.Deleting)
				_, err := admission.BeginDelete(ctx, db, row.ID)
				require.ErrorIs(t, err, admission.ErrBusy)
			}
			select {
			case <-service.ready:
			case <-attachDone:
				t.Fatalf("attach completed before the publication gate: %v", attachErr)
			case <-ctx.Done():
				t.Fatal("attach did not reach the publication gate")
			}
			requireProtected(initialRefs, 1)
			inFlightSpec, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, binding.SandboxID)
			require.NoError(t, err)
			require.Equal(t, []sandboxspec.RuntimeVolumeMount{previousMount}, inFlightSpec.Mounts)
			nodeRef, found, err := backend.holders.GetRecord(binding.Namespace, binding.VolumeID)
			require.NoError(t, err)
			require.True(t, found)
			require.NotNil(t, nodeRef.ReportPending)
			require.Equal(t, !tc.sharedHolder, *nodeRef.ReportPending)
			if tc.rejectWrite {
				// Check the trigger observes the reference update before it aborts the transaction.
				trigger := fmt.Sprintf(`CREATE TRIGGER reject_mount_write
					BEFORE UPDATE OF runtime_volume_mounts ON t_cube_sandbox_spec BEGIN
						SELECT CASE WHEN EXISTS (
							SELECT 1 FROM t_cube_volume WHERE id = %d AND refcount = 1 AND in_flight_creates = 0
						) THEN RAISE(ABORT, '%s')
						ELSE RAISE(ABORT, 'fixture reached mount write before ref settlement') END;
					END`, row.ID, mountWriteError)
				require.NoError(t, db.WithContext(ctx).Exec(trigger).Error)
			}
			if tc.deleteSpec {
				require.NoError(t, sandboxspec.Delete(ctx, binding.SandboxID))
			}
			unblock()
			select {
			case <-attachDone:
			case <-ctx.Done():
				t.Fatal("attach did not finish")
			}
			select {
			case <-service.finished:
			case <-ctx.Done():
				t.Fatal("fixture Update did not return")
			}
			switch {
			case tc.loseReply:
				require.Equal(t, codes.Unavailable, status.Code(attachErr))
			case tc.rejectWrite:
				require.ErrorContains(t, attachErr, mountWriteError)
			case tc.deleteSpec:
				require.ErrorIs(t, attachErr, sandboxspec.ErrSandboxSpecNotFound)
			default:
				require.NoError(t, attachErr)
			}
			requireProtected(tc.wantRefs, tc.wantBusy)
			if tc.deleteSpec {
				_, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, binding.SandboxID)
				require.ErrorIs(t, err, sandboxspec.ErrSandboxSpecNotFound)
				var oldSpec models.SandboxSpec
				require.NoError(t, db.WithContext(ctx).Unscoped().Take(&oldSpec, specBefore.RowID).Error)
				require.True(t, oldSpec.DeletedAt.Valid)
				require.Equal(t, specRowBefore.RuntimeVolumeMounts, oldSpec.RuntimeVolumeMounts)
				require.Equal(t, specRowBefore.RequestJSON, oldSpec.RequestJSON)
			} else {
				after, err := sandboxspec.GetWithRuntimeVolumeMounts(ctx, binding.SandboxID)
				require.NoError(t, err)
				require.Equal(t, specBefore.RowID, after.RowID)
				require.Equal(t, specBefore.Request, after.Request)
				wantMounts := []sandboxspec.RuntimeVolumeMount{previousMount}
				if tc.wantBusy == 0 {
					wantMounts = append(wantMounts, sandboxspec.RuntimeVolumeMount{
						VolumeID: row.VolumeID, VolumeRecordID: row.ID, ContainerPath: internalAttachTarget, Readonly: true,
					})
				}
				require.Equal(t, wantMounts, after.Mounts)
			}
			require.EqualValues(t, 1, service.calls.Load())
			require.Equal(t, 1, service.publications)
			require.EqualValues(t, 1-initialRefs, service.reports)
			require.Equal(t, binding, service.published)
			nodeRef, found, err = backend.holders.GetRecord(binding.Namespace, binding.VolumeID)
			require.NoError(t, err)
			require.True(t, found)
			require.NotNil(t, nodeRef.ReportPending)
			require.False(t, *nodeRef.ReportPending)
			require.Len(t, nodeRef.SandboxIDs, int(1+initialRefs))
			wantAttaches := []string{runtimeBackendOldVolume, runtimeBackendNewVolume}
			if tc.sharedHolder {
				require.Contains(t, nodeRef.SandboxIDs, firstHolderSandbox)
				wantAttaches = append(wantAttaches, runtimeBackendNewVolume)
			}
			require.Equal(t, wantAttaches, backend.driver.attached)
			require.Equal(t, initialRefs, backend.driver.lastAttach.RefCount)
			require.Equal(t, row.Driver, backend.driver.lastAttach.Driver)
			require.Equal(t, row.PrivateData, backend.driver.lastAttach.PrivateData)
			stored, err := backend.storage.readBackendFileInfo(ctx, binding.SandboxID)
			require.NoError(t, err)
			backend.requireMergedInfo(t, stored)
			holders, err := backend.holders.ListForSandbox(binding.SandboxID)
			require.NoError(t, err)
			require.ElementsMatch(t, []refcount.SandboxRecord{
				{Namespace: binding.Namespace, VolumeID: runtimeBackendOldVolume, RefCount: 1},
				{Namespace: binding.Namespace, VolumeID: binding.VolumeID, RefCount: 1 + initialRefs},
			}, holders)
		})
	}
}

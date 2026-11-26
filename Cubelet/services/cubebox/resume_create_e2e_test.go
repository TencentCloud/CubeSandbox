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
	"k8s.io/apimachinery/pkg/api/resource"

	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// A real Create-from-pause carries both the desired id and the pause snapshot
// id; the latter is what distinguishes it from an ordinary create with a
// desired-id annotation. The on-disk package loader is stubbed on the service
// so these tests exercise only the admission/release wiring in service.Create.
func resumeCreateRequest(id string) *cubeboxpb.RunCubeSandboxRequest {
	req := resumeReplaceRequest(id)
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-" + id
	return req
}

func resumableForCreateTest(id, cpu, mem string) *cubeboxstore.CubeBox {
	sb := pausedForReplace(id, sandboxstore.Endpoint{
		ShimSpawned:   true,
		ShimSpawnedAt: time.Now().Add(-time.Hour),
	})
	sb.ResourceWithOverHead = &cubeboxstore.ResourceWithOverHead{
		HostCpuQ: resource.MustParse(cpu),
		HostMemQ: resource.MustParse(mem),
	}
	return sb
}

func runningForCreateTest(id string) *cubeboxstore.CubeBox {
	sb := sandboxWithResourceForTest(id, cubeboxstore.Status{StartedAt: time.Now().UnixNano()},
		"1000m", "2Gi", 1, 0, 0)
	sb.SandboxID = id
	return sb
}

func resumeCreateService(t *testing.T, resuming *cubeboxstore.CubeBox,
	rows []*cubeboxstore.CubeBox, flow workflow.Flow,
	ratio float64, cpu int, mem string) (*service, *storeViewCubeboxAPI) {
	t.Helper()
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = ratio
	hostConf.Quota.Cpu = cpu
	hostConf.Quota.Mem = mem
	t.Cleanup(func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	})

	byID := make(map[string]*cubeboxstore.CubeBox, len(rows))
	for _, sb := range rows {
		byID[sb.ID] = sb
		byID[sb.SandboxID] = sb
	}
	store := &storeViewCubeboxAPI{byID: byID, list: rows}
	s := createServiceForTest(resuming, flow, 10*time.Minute)
	s.cubeboxMgr.cubeboxManger = store
	s.expandPauseSnapshotPackageFn = func(*cubeboxpb.RunCubeSandboxRequest) error { return nil }
	return s, store
}

// A rejection must reach the client as the capacity 409 (RetCode + RetMsg),
// never reach the create flow, and leave no reservation behind.
func TestCreateFromPausePropagatesAdmissionRejection(t *testing.T) {
	paused := resumableForCreateTest("sb-resume", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"),
		runningForCreateTest("run-3"), runningForCreateTest("run-4"), paused,
	}
	flow := &fakeCreateFlow{}
	s, _ := resumeCreateService(t, paused, rows, flow, 1.0, 16000, "8Gi")

	rsp, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))

	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp.Ret.RetCode)
	assert.Contains(t, rsp.Ret.RetMsg, "resume rejected by paused_resource_release_ratio policy")
	assert.Contains(t, rsp.Ret.RetMsg, "need 2048MB + used 8192MB > mem quota 8192MB")
	assert.Zero(t, flow.createCalls, "a rejected resume must not allocate anything")
	assert.Zero(t, flow.destroyCalls)
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
}

// defer release() must run on the success path.
func TestCreateFromPauseReleasesReservationOnSuccess(t *testing.T) {
	paused := resumableForCreateTest("sb-resume", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"),
		runningForCreateTest("run-3"), paused,
	}
	flow := &fakeCreateFlow{}
	s, _ := resumeCreateService(t, paused, rows, flow, 1.0, 16000, "8Gi")

	rsp, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))

	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 1, flow.createCalls)
	assert.Empty(t, s.cubeboxMgr.resumeReserved, "release must run when Create returns")
}

// defer release() must also run when the create flow fails, otherwise the
// failed sandbox keeps charging the node forever and blocks later resumes.
// (The panic path is covered by the same defer, but cannot be driven in a
// unit test: the production panic handler calls log.Fatalf, which exits.)
func TestCreateFromPauseReleasesReservationOnFlowFailure(t *testing.T) {
	paused := resumableForCreateTest("sb-resume", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"),
		runningForCreateTest("run-3"), paused,
	}
	flow := &fakeCreateFlow{createErr: errors.New("runContainer failed")}
	s, _ := resumeCreateService(t, paused, rows, flow, 1.0, 16000, "8Gi")

	rsp, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))

	require.NoError(t, err)
	assert.NotEqual(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 1, flow.createCalls)
	assert.Empty(t, s.cubeboxMgr.resumeReserved, "release must run on the error path too")
}

// The store row changes under a still-held reservation. Once the first create
// succeeds and releases, a second resume must see the persisted row rather
// than a stale reservation on top of it.
func TestCreateFromPauseReservationFollowsStoreRow(t *testing.T) {
	paused1 := resumableForCreateTest("paused-1", "1000m", "2Gi")
	paused2 := resumableForCreateTest("paused-2", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"), paused1, paused2,
	}
	flow := &fakeCreateFlow{}
	s, store := resumeCreateService(t, paused1, rows, flow, 1.0, 16000, "8Gi")
	flow.onCreate = func(*workflow.CreateContext) error {
		store.replaceRow(paused1.ID, cubeboxstore.Status{CreatedAt: time.Now().UnixNano()}, paused1)
		return nil
	}

	rsp1, err := s.Create(context.Background(), resumeCreateRequest(paused1.ID))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp1.Ret.RetCode)
	assert.Empty(t, s.cubeboxMgr.resumeReserved)

	rsp2, err := s.Create(context.Background(), resumeCreateRequest(paused2.ID))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Success, rsp2.Ret.RetCode,
		"4096MB running + 2048MB persisted + 2048MB need == 8192MB: this fits exactly")
	assert.Equal(t, 2, flow.createCalls)
}

// localCreateFlow drives the real local.Create as the engine's create step.
// CreateContext.PausedTombstone is assigned inside local.Create, so a fake
// flow can never make service.Create see one -- that is the e2e link the four
// cases above were missing.
//
// A second call is refused up front instead of falling into the real
// createContainers (which needs containerd); that turns "the retry was wrongly
// admitted" into an assertion failure rather than a panic.
type localCreateFlow struct {
	l           *local
	createCalls int
	tombstone   *cubeboxstore.CubeBox
}

func (f *localCreateFlow) ID() string                                     { return "local-create" }
func (f *localCreateFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (f *localCreateFlow) Create(ctx context.Context, opts *workflow.CreateContext) error {
	f.createCalls++
	if f.createCalls > 1 {
		return errors.New("create flow ran twice: the retry was not rejected by resume admission")
	}
	err := f.l.Create(ctx, opts)
	f.tombstone = opts.PausedTombstone
	return err
}
func (f *localCreateFlow) Destroy(context.Context, *workflow.DestroyContext) error { return nil }
func (f *localCreateFlow) CleanUp(context.Context, *workflow.CleanContext) error   { return nil }

// tombstoneStore is storeViewCubeboxAPI with the two writes a real PAUSED
// replacement performs made visible to the accounting snapshot: Delete drops
// the row, Save puts the restored tombstone back.
type tombstoneStore struct {
	*storeViewCubeboxAPI
	savedRows  []*cubeboxstore.CubeBox
	deletedIDs []string
	deleteErr  error
}

func (s *tombstoneStore) Save(_ context.Context, sb *cubeboxstore.CubeBox, _ ...cubes.UpdateCubeboxOpt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedRows = append(s.savedRows, sb)
	s.byID[sb.ID] = sb
	for i, cur := range s.list {
		if cur != nil && cur.ID == sb.ID {
			s.list[i] = sb
			return nil
		}
	}
	s.list = append(s.list, sb)
	return nil
}

func (s *tombstoneStore) Delete(_ context.Context, opt *cubes.DeleteOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedIDs = append(s.deletedIDs, opt.CubeboxID)
	delete(s.byID, opt.CubeboxID)
	kept := s.list[:0]
	for _, cur := range s.list {
		if cur != nil && cur.ID == opt.CubeboxID {
			continue
		}
		kept = append(kept, cur)
	}
	s.list = kept
	// Fail *after* the row is gone. That is exactly the window the tombstone
	// protects: no local record, and no replacement row saved yet.
	return s.deleteErr
}

func (s *tombstoneStore) addRow(sb *cubeboxstore.CubeBox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[sb.ID] = sb
	s.list = append(s.list, sb)
}

// Review point 2, end to end: a Create that reached local.Create really loses
// its PAUSED row, so a failure has to put a clean tombstone back. Otherwise
// the retry is classified as a cross-node restore, skips admission and
// silently overcommits the node.
func TestCreateFromPauseLocalCreateRestoresTombstoneAndRetriesUnderAdmission(t *testing.T) {
	paused := resumableForCreateTest("sb-tombstone-retry", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"),
		runningForCreateTest("run-3"), paused,
	}
	flow := &localCreateFlow{}
	// 3*2048MB running + 2048MB demand == 8192MB quota: admission passes, so
	// this request really reaches local.Create instead of being rejected early.
	s, view := resumeCreateService(t, paused, rows, flow, 1.0, 16000, "8Gi")
	flow.l = s.cubeboxMgr
	store := &tombstoneStore{
		storeViewCubeboxAPI: view,
		deleteErr:           errors.New("injected create failure after the paused row was removed"),
	}
	s.cubeboxMgr.cubeboxManger = store

	rsp1, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp1.Ret.RetCode,
		"PreConditionFailed short-circuits create-flow failover, so this drives the synchronous restore alone")
	assert.Contains(t, rsp1.Ret.RetMsg, "failed to replace paused sandbox")
	assert.Equal(t, 1, flow.createCalls)
	require.NotNil(t, flow.tombstone, "local.Create must hand the clean PAUSED snapshot to CreateContext")
	assert.Equal(t, []string{paused.ID}, store.deletedIDs)
	assert.Empty(t, s.cubeboxMgr.resumeReserved, "release() must run on the failure path")

	// service.Create put the tombstone back. The row is a clean PAUSED record:
	// no user-delete marker and no Removing flag, because the snapshot was
	// taken before local.Create wrote the marker.
	require.Len(t, store.savedRows, 1)
	restored := store.savedRows[0]
	assert.Equal(t, paused.ID, restored.ID)
	assert.Equal(t, paused.ID, restored.SandboxID)
	require.NotNil(t, restored.GetStatus())
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State())
	assert.Nil(t, restored.UserMarkDeletedTime)
	assert.False(t, restored.GetStatus().Get().Removing)
	row, getErr := store.Get(context.Background(), paused.ID)
	require.NoError(t, getErr)
	assert.Same(t, restored, row, "the retry must find the restored local record")

	// Spend the released quota on another sandbox: 4*2048MB now fills the
	// 8192MB node. A retry of the SAME resume must be rejected by admission,
	// not admitted as a cross-node restore the way a missing local row would.
	store.addRow(runningForCreateTest("run-4"))
	rsp2, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp2.Ret.RetCode)
	assert.Contains(t, rsp2.Ret.RetMsg, "resume rejected by paused_resource_release_ratio policy")
	assert.Contains(t, rsp2.Ret.RetMsg, "need 2048MB + used 8192MB > mem quota 8192MB")
	assert.Equal(t, 1, flow.createCalls, "a rejected retry must never reach the create flow again")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
}

// If Create-from-pause fails in an earlier workflow step (e.g. storage/network)
// before local.Create runs, failover's Destroy must NOT delete the existing
// PAUSED row from cubeboxStore — it must clean up the earlier step's partial
// allocations synchronously and keep the PAUSED tombstone intact so a retry
// still goes through resume admission.
func TestCreateFromPauseEarlyStepFailurePreservesPausedTombstone(t *testing.T) {
	paused := resumableForCreateTest("sb-early-step-fail", "1000m", "2Gi")
	rows := []*cubeboxstore.CubeBox{
		runningForCreateTest("run-1"), runningForCreateTest("run-2"),
		runningForCreateTest("run-3"), paused,
	}
	earlyFailFlow := &fakeCreateFlow{createErr: errors.New("storage create failed")}
	s, view := resumeCreateService(t, paused, rows, earlyFailFlow, 1.0, 16000, "8Gi")
	store := &tombstoneStore{storeViewCubeboxAPI: view}
	s.cubeboxMgr.cubeboxManger = store
	s.engine.AddFlow("destroy", &workflow.Workflow{
		Name:    "destroy",
		Limiter: semaphore.NewLimiter(2),
		Steps: []*workflow.Step{
			{Name: "cubebox", Actions: []workflow.Flow{s.cubeboxMgr}},
			{Name: "resources", Actions: []workflow.Flow{earlyFailFlow}},
		},
	})

	rsp1, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))
	require.NoError(t, err)
	assert.NotEqual(t, errorcode.ErrorCode_Success, rsp1.Ret.RetCode)
	assert.Equal(t, 1, earlyFailFlow.createCalls)
	assert.Equal(t, 1, earlyFailFlow.destroyCalls,
		"failover must finish synchronously before Create returns")
	assert.Empty(t, store.deletedIDs, "early-step failover must not delete the PAUSED tombstone")

	row, getErr := store.Get(context.Background(), paused.ID)
	require.NoError(t, getErr)
	require.NotNil(t, row)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, row.GetStatus().Get().State())

	// Fill the node and verify the retry is still caught by admission.
	store.addRow(runningForCreateTest("run-4"))
	rsp2, err := s.Create(context.Background(), resumeCreateRequest(paused.ID))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp2.Ret.RetCode)
	assert.Contains(t, rsp2.Ret.RetMsg, "need 2048MB + used 8192MB > mem quota 8192MB")
	assert.Equal(t, 1, earlyFailFlow.createCalls)
}

func TestCreateFromPauseWithoutCoordinatorRetainsLegacyFailover(t *testing.T) {
	paused := resumableForCreateTest("sb-legacy-resume", "1000m", "2Gi")
	flow := &fakeCreateFlow{onCreate: func(opts *workflow.CreateContext) error {
		require.Nil(t, opts.RollbackCoordinator, "no durable owner must mean ordinary workflow failover")
		require.True(t, opts.Failover)
		return nil
	}}
	s, _ := resumeCreateService(t, paused, []*cubeboxstore.CubeBox{paused}, flow, 1.0, 16000, "8Gi")
	require.Nil(t, s.rollbacks)
	rsp, err := s.Create(t.Context(), resumeCreateRequest(paused.ID))
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode())
	require.Equal(t, 1, flow.createCalls)
	require.Empty(t, s.cubeboxMgr.resumeReserved)
}

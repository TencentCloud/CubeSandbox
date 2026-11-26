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

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cbri"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/runc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// failingCreateFlow fails the create step (so service.Create rolls back) and
// records whether the rollback destroy ran — plus the DestroyContext it saw,
// so the test can assert RollbackKeepsRow propagated.
type failingCreateFlow struct {
	createErr    error
	createCalls  int
	destroyCalls int
	lastDestroy  *workflow.DestroyContext
	// skipIDPin models a failure that leaves createInfo.SandboxID unset —
	// the flow never got past the workflow limiter / createid step, so there
	// is nothing to tear down and the synchronous rollback must stay off.
	skipIDPin bool
}

func (f *failingCreateFlow) ID() string                                     { return "failing-create" }
func (f *failingCreateFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (f *failingCreateFlow) Create(_ context.Context, opts *workflow.CreateContext) error {
	f.createCalls++
	if opts.SandboxID == "" {
		// Mimic the createid step, which pins the id before any allocating
		// step runs — unless the test models a failure that predates it.
		opts.SandboxID = opts.ReqInfo.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID]
	}
	if f.skipIDPin {
		opts.SandboxID = ""
	}
	return f.createErr
}
func (f *failingCreateFlow) Destroy(_ context.Context, opts *workflow.DestroyContext) error {
	f.destroyCalls++
	f.lastDestroy = opts
	return nil
}
func (f *failingCreateFlow) CleanUp(context.Context, *workflow.CleanContext) error { return nil }

// createRollbackServiceForTest wires service.Create to a create flow that
// fails, so the synchronous Create-from-pause rollback path runs end to end
// (gate snapshot → engine.Rollback destroy → afterDestroy tombstone restore).
func createRollbackServiceForTest(sb *cubeboxstore.CubeBox, flow *failingCreateFlow) (*service, *fakeCubeboxAPI) {
	engine := &workflow.Engine{}
	mk := func(name string) *workflow.Workflow {
		wf := &workflow.Workflow{Name: name, Limiter: semaphore.NewLimiter(1)}
		wf.AppendStep(&workflow.Step{Name: flow.ID(), Actions: []workflow.Flow{flow}})
		return wf
	}
	engine.AddFlow("create", mk("create"))
	engine.AddFlow("destroy", mk("destroy"))
	engine.AddCleaupFlow(flow)

	mgr := &fakeCubeboxAPI{cb: sb}
	local := &local{
		config: &CubeConfig{
			DefaultRuntimeName: "io.containerd.cube.v2.task",
			Runtimes: map[string]cubeconfig.Runtime{
				"io.containerd.cube.v2.task": {Type: "io.containerd.cube.v2.task"},
			},
		},
		cubeboxManger: mgr,
	}
	local.SetShimIntentTTL(time.Minute)
	return &service{
		engine:                engine,
		cubeboxMgr:            local,
		sandboxLifecycleLocks: utils.NewResourceLocks(),
	}, mgr
}

// TestCreateFromPauseRollsBackSynchronouslyAndRestoresTombstone is the
// end-to-end pin for the synchronous-rollback refactor: when the create flow
// fails after the gate captured the tombstone, service.Create must — before it
// returns and while it still holds the lifecycle lock — run the rollback
// destroy (keeping the row) and save the tombstone snapshot back, scrubbed of
// any delete mark. The retry then sees exactly the pre-attempt record.
func TestCreateFromPauseRollsBackSynchronouslyAndRestoresTombstone(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 8000
	hostConf.Quota.Mem = "16Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// The thin Create now carries the annotation pair, so expansion reads the
	// seeded pause package before the gate and admission run.
	seedPausePackageForTest(t, "snap-test", resumePackedSpecForTest())

	// A PAUSED tombstone whose shim is provably gone, so the gate passes and
	// the failure happens later, inside the create flow.
	tomb := sandboxWithResourceForTest("sb-syncrollback", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Hour).UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	tomb.SandboxID = "sb-syncrollback"

	flow := &failingCreateFlow{createErr: errors.New("rootfs mount failed")}
	s, mgr := createRollbackServiceForTest(tomb, flow)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-syncrollback"))
	require.NoError(t, err)
	assert.NotEqual(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode,
		"the failed create must surface its error")

	assert.Equal(t, 1, flow.createCalls, "the create flow ran and failed")
	assert.Equal(t, 1, flow.destroyCalls,
		"the rollback must tear the failed attempt down synchronously, inside Create")
	require.NotNil(t, flow.lastDestroy)
	assert.True(t, flow.lastDestroy.IsRollBack, "the rollback destroy carries IsRollBack")
	assert.True(t, flow.lastDestroy.RollbackKeepsRow,
		"the rollback destroy must keep the row for the tombstone restore (#2042)")

	// The tombstone is back over whatever the attempt left, PAUSED, marks
	// scrubbed — synchronously, by the time Create returned.
	restored, gerr := mgr.Get(context.Background(), "sb-syncrollback")
	require.NoError(t, gerr)
	require.NotNil(t, restored)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, restored.GetStatus().Get().State(),
		"the restore must put the PAUSED tombstone back before Create returns")
	assert.Equal(t, tomb.CreatedAt, restored.CreatedAt, "the restored row is the pre-attempt tombstone")
	assert.Nil(t, restored.UserMarkDeletedTime, "the restored tombstone must not read as user-deleted")
	assert.Empty(t, restored.DeleteRequestID)
	assert.Zero(t, mgr.deleteCalls, "the rollback must never delete the tombstone row")

	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "the deferred release must drop the reservation on the failure path")
}

// TestCreateFromPausePreConditionFailedSkipsRollback pins the exclusion that
// mirrors engine.run: a PreConditionFailed create error marks a state the flow
// refused to act on (e.g. a collision), not a half-built sandbox, so the
// rollback must NOT run and tear down a live sandbox.
func TestCreateFromPausePreConditionFailedSkipsRollback(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 8000
	hostConf.Quota.Mem = "16Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	seedPausePackageForTest(t, "snap-test", resumePackedSpecForTest())
	tomb := sandboxWithResourceForTest("sb-precond", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Hour).UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	tomb.SandboxID = "sb-precond"

	flow := &failingCreateFlow{createErr: ret.Err(errorcode.ErrorCode_PreConditionFailed, "collision")}
	s, _ := createRollbackServiceForTest(tomb, flow)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-precond"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp.Ret.RetCode)
	assert.Equal(t, 1, flow.createCalls)
	assert.Zero(t, flow.destroyCalls,
		"PreConditionFailed must not trigger the rollback (it would destroy a live sandbox)")
}

// TestCreateFromPauseSkipsRollbackWhenTheFlowNeverStarted pins the guard the
// bot review asked for: engine.run rejects with ConcurrentFailed when the
// create workflow's semaphore is saturated — BEFORE the createid step pins
// createInfo.SandboxID and before any step allocates. Rolling such an attempt
// back would run the destroy workflow against an empty sandbox id; the
// trigger must skip it (the tombstone was never touched, so there is nothing
// to restore either).
func TestCreateFromPauseSkipsRollbackWhenTheFlowNeverStarted(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 8000
	hostConf.Quota.Mem = "16Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	seedPausePackageForTest(t, "snap-test", resumePackedSpecForTest())
	tomb := sandboxWithResourceForTest("sb-norollback", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().Add(-time.Hour).UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	tomb.SandboxID = "sb-norollback"

	flow := &failingCreateFlow{
		createErr: ret.Errorf(errorcode.ErrorCode_ConcurrentFailed, "flow [create] exceed limited"),
		skipIDPin: true, // the failure predates the createid step
	}
	s, mgr := createRollbackServiceForTest(tomb, flow)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-norollback"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_ConcurrentFailed, rsp.Ret.RetCode,
		"a limiter rejection must surface as the cheap retriable backpressure it is")
	assert.Equal(t, 1, flow.createCalls)
	assert.Zero(t, flow.destroyCalls,
		"nothing was allocated: the rollback must not run the destroy workflow on an empty id")
	assert.Zero(t, mgr.deleteCalls)

	// The tombstone is untouched — the retry finds exactly what it expects.
	cur, gerr := mgr.Get(context.Background(), "sb-norollback")
	require.NoError(t, gerr)
	require.NotNil(t, cur)
	assert.Equal(t, cubeboxpb.ContainerState_CONTAINER_PAUSED, cur.GetStatus().Get().State())
	assert.Nil(t, cur.UserMarkDeletedTime)
}

// stubCbriManager stands in for the cbri API manager local.Destroy calls
// before touching any container runtime, so the row-keep guard can be driven
// through the REAL local.Destroy.
type stubCbriManager struct{ destroyCalls int }

func (s *stubCbriManager) SetCubeRuntimeImplementation(cbri.CubeRuntimeImplementation) {
}
func (s *stubCbriManager) GetPassthroughMounts(context.Context, *workflow.CreateContext) ([]specs.Mount, error) {
	return nil, nil
}
func (s *stubCbriManager) CreateSandbox(context.Context, *workflow.CreateContext) ([]oci.SpecOpts, error) {
	return nil, nil
}
func (s *stubCbriManager) CreateContainer(context.Context, *cubeboxstore.CubeBox, *cubeboxstore.Container) ([]oci.SpecOpts, error) {
	return nil, nil
}
func (s *stubCbriManager) PostCreateContainer(context.Context, *cubeboxstore.CubeBox, *cubeboxstore.Container) error {
	return nil
}
func (s *stubCbriManager) DestroySandbox(context.Context, string) error {
	s.destroyCalls++
	return nil
}

// TestLocalDestroyRollbackKeepsRowSkipsDeleteAndStamp pins the row-keep
// invariant through the real local.Destroy — the guarantee the whole
// synchronous-restore design rests on: with RollbackKeepsRow the deferred
// store Delete must not run (deleting the row would open the #2042
// absent-row window the restore is supposed to close) and the IsRollBack
// UserMarkDeletedTime stamp must not be written (a marked tombstone is one
// gcservice.allowCleanup hands to the cleanup flow, reclaiming the pause
// package the retry needs). The negative case pins that an ordinary rollback
// destroy without the flag still stamps and deletes.
func TestLocalDestroyRollbackKeepsRowSkipsDeleteAndStamp(t *testing.T) {
	newDebrisRow := func(id string) *cubeboxstore.CubeBox {
		// The post-Save rollback shape: this attempt's row (not paused). Its
		// containers are pre-marked deleted so the container loop skips the
		// containerd teardown — this test drives the deferred row guard, not
		// the runtime teardown.
		sb := newCubeboxWithStatusForTest(id, cubeboxstore.Status{
			CreatedAt: time.Now().UnixNano(),
			StartedAt: time.Now().UnixNano(),
		})
		sb.SandboxID = id
		now := time.Now()
		for _, c := range sb.AllContainers() {
			c.DeletedTime = &now
		}
		return sb
	}
	rollbackOpts := func(id string, keepsRow bool) *workflow.DestroyContext {
		opts := &workflow.DestroyContext{
			DestroyInfo: &cubeboxpb.DestroyCubeSandboxRequest{
				SandboxID: id,
				RequestID: "rollback-request",
			},
			RollbackKeepsRow: keepsRow,
		}
		opts.SandboxID = id
		opts.IsRollBack = true
		return opts
	}

	// local.Destroy ends with runc.Clean, which needs the runc data dir
	// initialized (nil-safe in production init, empty in unit tests).
	runc.Init(t.TempDir())

	t.Run("keeps the row and skips the stamp", func(t *testing.T) {
		sb := newDebrisRow("sb-keepsrow")
		mgr := &fakeCubeboxAPI{cb: sb}
		cbri := &stubCbriManager{}
		l := &local{cubeboxManger: mgr, cbriManager: cbri}

		require.NoError(t, l.Destroy(context.Background(), rollbackOpts("sb-keepsrow", true)))

		assert.Equal(t, 1, cbri.destroyCalls, "the runtime teardown itself must run")
		assert.Zero(t, mgr.deleteCalls,
			"the rollback destroy must keep the row for the synchronous tombstone restore (#2042)")
		assert.Nil(t, sb.UserMarkDeletedTime,
			"a kept row must not carry the IsRollBack stamp (allowCleanup would reclaim the pause package)")
		assert.Empty(t, sb.DeleteRequestID)
		assert.Empty(t, mgr.syncIDs, "nothing to persist: no stamp was written")
	})

	t.Run("without the flag the rollback stamp and delete proceed", func(t *testing.T) {
		sb := newDebrisRow("sb-ordinary-rollback")
		mgr := &fakeCubeboxAPI{cb: sb}
		l := &local{cubeboxManger: mgr, cbriManager: &stubCbriManager{}}

		require.NoError(t, l.Destroy(context.Background(), rollbackOpts("sb-ordinary-rollback", false)))

		assert.Equal(t, 1, mgr.deleteCalls, "an ordinary rollback destroy deletes its debris row as before")
		require.NotNil(t, sb.UserMarkDeletedTime, "the IsRollBack stamp lets the CDP delete hooks pass")
		assert.Equal(t, "rollback-request", sb.DeleteRequestID)
	})
}

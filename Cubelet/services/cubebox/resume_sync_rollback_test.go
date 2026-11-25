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

	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
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
}

func (f *failingCreateFlow) ID() string                                     { return "failing-create" }
func (f *failingCreateFlow) Init(context.Context, *workflow.InitInfo) error { return nil }
func (f *failingCreateFlow) Create(context.Context, *workflow.CreateContext) error {
	f.createCalls++
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

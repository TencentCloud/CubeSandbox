// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// fakeDestroyContainer overrides Task and inherits the unused containerd
// operations from its embedded interface.
type fakeDestroyContainer struct {
	containerd.Container

	task      containerd.Task
	taskErr   error
	taskCalls int
}

func (f *fakeDestroyContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	f.taskCalls++
	return f.task, f.taskErr
}

type destroyRecordingFlow struct {
	destroyCalls       int
	cleanupCalls       int
	destroyErr         error
	destroyDeadline    time.Time
	hasDestroyDeadline bool
}

func (f *destroyRecordingFlow) ID() string { return "destroy-recording-flow" }

func (f *destroyRecordingFlow) Init(context.Context, *workflow.InitInfo) error { return nil }

func (f *destroyRecordingFlow) Create(context.Context, *workflow.CreateContext) error { return nil }

func (f *destroyRecordingFlow) Destroy(ctx context.Context, _ *workflow.DestroyContext) error {
	f.destroyCalls++
	f.destroyDeadline, f.hasDestroyDeadline = ctx.Deadline()
	return f.destroyErr
}

func (f *destroyRecordingFlow) CleanUp(context.Context, *workflow.CleanContext) error {
	f.cleanupCalls++
	return nil
}

func newDestroyLifecycleServiceForTest(sb *cubeboxstore.CubeBox) (*service, *fakeCubeboxAPI, *destroyRecordingFlow) {
	flow := &destroyRecordingFlow{}
	engine := &workflow.Engine{}
	engine.AddFlow("destroy", &workflow.Workflow{
		Name:    "destroy",
		Limiter: semaphore.NewLimiter(1),
		Steps: []*workflow.Step{{
			Name:    flow.ID(),
			Actions: []workflow.Flow{flow},
		}},
	})
	engine.AddFlow("cleanup", &workflow.Workflow{
		Name:    "cleanup",
		Limiter: semaphore.NewLimiter(1),
		Steps: []*workflow.Step{{
			Name:    flow.ID(),
			Actions: []workflow.Flow{flow},
		}},
	})
	engine.AddCleaupFlow(flow)

	manager := &fakeCubeboxAPI{cb: sb}
	return &service{
		engine: engine,
		cubeboxMgr: &local{
			config: &CubeConfig{
				DefaultRuntimeName: "io.containerd.cube.v2.task",
			},
			cubeboxManger: manager,
		},
		config: &ServicesConfig{
			destroyDeadline: 30 * time.Second,
		},
		sandboxLifecycleLocks: utils.NewResourceLocks(),
	}, manager, flow
}

func destroySandboxForTest(t *testing.T, s *service, sandboxID string) *cubebox.DestroyCubeSandboxResponse {
	return destroySandboxWithContextForTest(t, s, context.Background(), sandboxID)
}

func destroySandboxWithContextForTest(t *testing.T, s *service, ctx context.Context, sandboxID string) *cubebox.DestroyCubeSandboxResponse {
	t.Helper()
	rsp, err := s.Destroy(ctx, &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sandboxID,
		RequestID: "delete-request",
	})
	require.NoError(t, err)
	return rsp
}

func TestDestroyRunningSandboxDoesNotResume(t *testing.T) {
	sb := newCubeboxWithStatusForTest("running-delete", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	container := &fakeDestroyContainer{}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyLifecycleServiceForTest(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requestDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	rsp := destroySandboxWithContextForTest(t, s, ctx, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Zero(t, container.taskCalls, "a running sandbox must take the existing destroy path directly")
	assert.Equal(t, 1, flow.destroyCalls)
	require.True(t, flow.hasDestroyDeadline)
	assert.Equal(t, requestDeadline, flow.destroyDeadline,
		"running DELETE must keep its existing deadline")
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "delete-request", sb.DeleteRequestID)
	assert.Equal(t, []string{sb.ID}, manager.syncIDs, "only the normal delete marker is persisted")
}

func TestDestroyPausedSandboxDoesNotResume(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	container := &fakeDestroyContainer{task: task}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyLifecycleServiceForTest(sb)

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Zero(t, task.resumeCalls, "CoW pause has no shim; Destroy must not resume before delete")
	assert.Zero(t, container.taskCalls)
	assert.Equal(t, 1, flow.destroyCalls)
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "delete-request", sb.DeleteRequestID)
	assert.Equal(t, []string{sb.ID}, manager.syncIDs)
}

func TestDestroyPausedKeepTombstoneDoesNotResume(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-keep-tombstone", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	container := &fakeDestroyContainer{task: task}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyLifecycleServiceForTest(sb)

	rsp, err := s.Destroy(context.Background(), &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		RequestID: "pause-cleanup-request",
		Annotations: map[string]string{
			constants.AnnotationPauseKeepTombstone: "true",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Zero(t, task.resumeCalls)
	assert.Equal(t, 1, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime, "keep_tombstone clears the temporary delete marker after wipe")
	assert.Equal(t, []string{sb.ID, sb.ID}, manager.syncIDs)
	assert.NotZero(t, sb.GetStatus().Get().PausedAt, "keep_tombstone must leave PAUSED metadata")
}

func TestDestroyRunningSandboxReturnsRetryableErrorWhenLifecycleLockIsBusy(t *testing.T) {
	sb := newCubeboxWithStatusForTest("locked-running-delete", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, _, flow := newDestroyLifecycleServiceForTest(sb)
	unlock := s.sandboxLifecycleLocks.Lock(sb.ID)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rsp, err := s.Destroy(ctx, &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		RequestID: "locked-delete-request",
	})
	require.NoError(t, err)

	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, rsp.Ret.RetCode)
	assert.Equal(t, "sandbox lifecycle operation is in progress; retry DELETE after 2 seconds", rsp.Ret.RetMsg)
	assert.Zero(t, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)
}

func TestDestroyReturnsRetryWhenResponseReserveIsExhausted(t *testing.T) {
	sb := newCubeboxWithStatusForTest("deadline-exhausted-delete", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, _, flow := newDestroyLifecycleServiceForTest(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rsp, err := s.Destroy(ctx, &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		RequestID: "deadline-exhausted-delete-request",
	})
	require.NoError(t, err)

	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, rsp.Ret.RetCode)
	assert.Equal(t,
		"cannot start delete: insufficient time remains for the Cubelet RPC response; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Zero(t, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)
}

func TestDestroyDebugCleanupPreservesDeleteMarker(t *testing.T) {
	sb := newCubeboxWithStatusForTest("debug-cleanup", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, manager, flow := newDestroyLifecycleServiceForTest(sb)

	rsp, err := s.Destroy(context.Background(), &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		RequestID: "debug-cleanup-request",
		Annotations: map[string]string{
			"cube.debug.cleanup": "true",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 1, flow.cleanupCalls)
	assert.Zero(t, flow.destroyCalls)
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "debug-cleanup-request", sb.DeleteRequestID)
	assert.Equal(t, []string{sb.ID}, manager.syncIDs)
}

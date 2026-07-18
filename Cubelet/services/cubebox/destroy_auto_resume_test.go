// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
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

func newDestroyAutoResumeServiceForTest(sb *cubeboxstore.CubeBox) (*service, *fakeCubeboxAPI, *destroyRecordingFlow) {
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

func TestDestroyAutoResumeLeavesResponseReserveForWorkflowDestroy(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-response-reserve", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s, _, flow := newDestroyAutoResumeServiceForTest(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requestDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	rsp := destroySandboxWithContextForTest(t, s, ctx, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	require.True(t, flow.hasDestroyDeadline)
	assert.WithinDuration(t, requestDeadline.Add(-deleteResponseReserveMax), flow.destroyDeadline, time.Millisecond)
}

func TestDestroyAutoResumesPausedSandboxBeforeWorkflowDestroy(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	container := &fakeDestroyContainer{task: task}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 1, container.taskCalls)
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, 1, flow.destroyCalls, "normal destroy must follow a successful internal resume")
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "delete-request", sb.DeleteRequestID)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	require.Len(t, manager.syncSnapshots, 2)
	assert.Equal(t, []string{sb.ID, sb.ID}, manager.syncIDs)
	assert.Equal(t, int64(0), manager.syncSnapshots[0].pausedAt)
	assert.NotZero(t, manager.syncSnapshots[0].startedAt)
	assert.False(t, manager.syncSnapshots[0].userMarkedDeleted,
		"the resumed RUNNING state must be durable before the delete marker")
	assert.True(t, manager.syncSnapshots[1].userMarkedDeleted)
}

func TestDestroyDoesNotMarkPausedSandboxDeletedWhenAutoResumeFails(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-failure", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{
		resumeErr: errors.New("shim timed out"),
		status:    containerd.Paused,
	}
	container := &fakeDestroyContainer{task: task}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, rsp.Ret.RetCode)
	assert.Equal(t,
		"failed to resume paused sandbox before delete: shim timed out; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, 1, task.statusCalls, "an uncertain resume must reconcile once")
	assert.Zero(t, flow.destroyCalls, "destroy must not start until RUNNING is proven")
	assert.Nil(t, sb.UserMarkDeletedTime, "failed auto-resume must leave DELETE retryable")
	assert.Empty(t, manager.syncIDs, "a failed preflight must not persist a delete marker")
}

func TestDestroyDoesNotMarkPausedSandboxDeletedWhenTaskCannotBeLoaded(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-no-task", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	sb.FirstContainer().Container = nil
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, rsp.Ret.RetCode)
	assert.Equal(t,
		"failed to resume paused sandbox before delete: failed to load task for paused sandbox; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Zero(t, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)
	assert.Empty(t, manager.syncIDs)
}

func TestDestroyDoesNotMarkPausedSandboxDeletedWhenTaskLookupFails(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-task-error", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	container := &fakeDestroyContainer{taskErr: errors.New("containerd task lookup failed")}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, rsp.Ret.RetCode)
	assert.Equal(t,
		"failed to resume paused sandbox before delete: containerd task lookup failed; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Equal(t, 1, container.taskCalls)
	assert.Zero(t, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)
	assert.Empty(t, manager.syncIDs)
}

func TestDestroyDoesNotDestroyWhenPersistingAutoResumedStateFails(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-persist-failure", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)
	manager.syncErr = errors.New("metadata store unavailable")

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, rsp.Ret.RetCode)
	assert.Equal(t,
		"failed to persist resumed sandbox before delete: metadata store unavailable; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Equal(t, 1, task.resumeCalls)
	assert.Zero(t, flow.destroyCalls, "destroy must not begin without a durable resumed state")
	assert.Nil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, []string{sb.ID}, manager.syncIDs)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt)
}

func TestDestroyKeepsAutoResumedSandboxMarkedWhenWorkflowDestroyFails(t *testing.T) {
	sb := newCubeboxWithStatusForTest("paused-delete-destroy-failure", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)
	flow.destroyErr = errors.New("workflow destroy failed")

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Unknown, rsp.Ret.RetCode)
	assert.Equal(t, "workflow destroy failed", rsp.Ret.RetMsg)
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, 1, flow.destroyCalls)
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "delete-request", sb.DeleteRequestID)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt)
	assert.Equal(t, []string{sb.ID, sb.ID}, manager.syncIDs)
}

func TestDestroyRunningSandboxDoesNotResume(t *testing.T) {
	sb := newCubeboxWithStatusForTest("running-delete", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	container := &fakeDestroyContainer{}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

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

func TestDestroyReloadsPausedStateAfterAcquiringLifecycleLock(t *testing.T) {
	sb := newCubeboxWithStatusForTest("running-then-paused-delete", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeDestroyTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)
	paused := sb.DeepCopy()
	paused.FirstContainer().Container = sb.FirstContainer().Container
	manager.getHook = func(call int) {
		if call != 2 {
			return
		}
		require.NoError(t, paused.GetStatus().Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.StartedAt = 0
			status.PausedAt = time.Now().UnixNano()
			return status, nil
		}))
		manager.cb = paused
	}

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
	assert.Equal(t, 3, manager.getCalls, "Destroy must reload after acquiring the lifecycle lock")
	assert.Equal(t, 1, task.resumeCalls, "the lock-protected read must trigger paused preflight")
	assert.Equal(t, 1, flow.destroyCalls)
}

func TestDestroyRunningSandboxReturnsRetryableErrorWhenLifecycleLockIsBusy(t *testing.T) {
	sb := newCubeboxWithStatusForTest("locked-running-delete", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, _, flow := newDestroyAutoResumeServiceForTest(sb)
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

func TestDestroyReturnsLongRetryWhenResponseReserveIsExhausted(t *testing.T) {
	sb := newCubeboxWithStatusForTest("deadline-exhausted-delete", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, _, flow := newDestroyAutoResumeServiceForTest(sb)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rsp, err := s.Destroy(ctx, &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sb.ID,
		RequestID: "deadline-exhausted-delete-request",
	})
	require.NoError(t, err)

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, rsp.Ret.RetCode)
	assert.Equal(t,
		"cannot start delete: insufficient time remains for the Cubelet RPC response; retry DELETE after 5 seconds",
		rsp.Ret.RetMsg)
	assert.Zero(t, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)
}

func TestDestroyPausedSandboxSucceedsWhenCapacityWouldRejectResume(t *testing.T) {
	sb := sandboxWithResourceForTest("paused-delete-overcommit", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	task := &fakeDestroyTask{}
	container := &fakeDestroyContainer{task: task}
	sb.FirstContainer().Container = container
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	rsp := destroySandboxForTest(t, s, sb.ID)

	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode,
		"delete must succeed even when the node cannot admit a normal resume")
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, 1, flow.destroyCalls)
	require.NotNil(t, sb.UserMarkDeletedTime)
	assert.Equal(t, "delete-request", sb.DeleteRequestID)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt)
	assert.Len(t, manager.syncIDs, 2)
}

func TestDestroyDebugCleanupPreservesDeleteMarker(t *testing.T) {
	sb := newCubeboxWithStatusForTest("debug-cleanup", cubeboxstore.Status{
		StartedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	s, manager, flow := newDestroyAutoResumeServiceForTest(sb)

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

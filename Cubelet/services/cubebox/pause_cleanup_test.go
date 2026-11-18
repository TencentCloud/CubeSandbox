// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeletlog "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// pauseCleanupTestContext carries the request trace the workflow engine needs.
func pauseCleanupTestContext() context.Context {
	rt := &CubeLog.RequestTrace{
		Action:       "PauseCleanupTest",
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       "workflow",
		CalleeAction: "destroy",
	}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	return cubeletlog.WithLogger(ctx, cubeletlog.NewWrapperLogEntry(cubeletlog.AuditLogger.WithContext(ctx)))
}

func stubStoragePluginVolumes(t *testing.T, fn func(context.Context, string) (bool, error)) func() {
	t.Helper()
	orig := storageSandboxHasPluginVolumes
	storageSandboxHasPluginVolumes = fn
	return func() { storageSandboxHasPluginVolumes = orig }
}

// markPauseCleanupPending is the row state of an unfinished pause teardown.
func markPauseCleanupPending(sb *cubeboxstore.CubeBox, requestID string) {
	now := time.Now()
	sb.Lock()
	defer sb.Unlock()
	sb.UserMarkDeletedTime = &now
	sb.DeleteRequestID = requestID
}

// A finished pause must not be reaped again; only a leftover marker triggers it.
func TestPauseCleanupRunsOnlyWhenMarkerLeft(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-clean", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s, _, flow := newDestroyLifecycleServiceForTest(sb)

	require.NoError(t, s.ensurePauseCleanupFinished(pauseCleanupTestContext(), sb.ID, "resume"))
	assert.Zero(t, flow.destroyCalls, "a pause that finished needs no second teardown")

	markPauseCleanupPending(sb, "pause-request")
	require.NoError(t, s.ensurePauseCleanupFinished(pauseCleanupTestContext(), sb.ID, "resume"))
	assert.Equal(t, 1, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime, "a successful reap clears the marker")
}

// The context Create passes here: a RequestTrace and namespace, no
// installed logger.
func TestPauseCleanupRunsFromCreateShapedContext(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-create-ctx", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	markPauseCleanupPending(sb, "pause-request")
	s, _, flow := newDestroyLifecycleServiceForTest(sb)

	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{
		Action:       "Create",
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       "workflow",
		CalleeAction: "create",
	})
	ctx = namespaces.WithNamespace(ctx, namespaces.Default)

	require.NoError(t, s.ensurePauseCleanupFinished(ctx, sb.ID, "resume"))
	assert.Equal(t, 1, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime, "a successful reap clears the marker")
}

// An unreadable row must fail the resume, not skip the teardown.
func TestPauseCleanupUnreadableRowFailsClosed(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-read-err", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	markPauseCleanupPending(sb, "pause-request")
	s, manager, flow := newDestroyLifecycleServiceForTest(sb)
	manager.getErr = errors.New("store read failed")

	err := s.ensurePauseCleanupFinished(pauseCleanupTestContext(), sb.ID, "resume")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store read failed")
	assert.Zero(t, flow.destroyCalls, "an unreadable row must not be treated as a finished pause")
	assert.NotNil(t, sb.UserMarkDeletedTime)
}

// The reply does not wait for the teardown; the reaper finishes it.
func TestDestroyLiveAfterPauseDefersWithoutPluginVolumes(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-pause-async", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s, _, flow := newDestroyLifecycleServiceForTest(sb)
	restore := stubStoragePluginVolumes(t, func(context.Context, string) (bool, error) { return false, nil })
	defer restore()
	reaped := make(chan struct{})
	flow.onDestroy = func() { close(reaped) }

	ext, err := s.destroyLiveAfterPause(pauseCleanupTestContext(), &cubebox.UpdateCubeSandboxRequest{
		RequestID: "pause-request",
		SandboxID: sb.ID,
	}, sb, false)
	require.NoError(t, err)
	assert.Nil(t, ext, "a deferred destroy has no volume-ref events to report")

	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("the reaper did not run")
	}
	// The marker is cleared after the destroy returns, so wait for that too.
	require.Eventually(t, func() bool {
		sb.Lock()
		defer sb.Unlock()
		return sb.UserMarkDeletedTime == nil
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, flow.destroyCalls)
}

// A resume that wins the lock reaps the sandbox itself; this run must no-op.
func TestPauseCleanupReaperLeavesReapedSandboxAlone(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-reaper", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	markPauseCleanupPending(sb, "pause-request")
	s, _, flow := newDestroyLifecycleServiceForTest(sb)

	unlock, err := s.sandboxLifecycleLocks.LockContext(context.Background(), sb.ID)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		s.reapPausedSandbox(pauseCleanupTestContext(), sb.ID)
		close(done)
	}()
	// The resume won the lock and finished the teardown itself.
	sb.Lock()
	sb.UserMarkDeletedTime = nil
	sb.Unlock()
	unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reaper did not return")
	}
	assert.Zero(t, flow.destroyCalls, "a reaped sandbox must not be destroyed a second time")
}

// Plugin volumes (volume-ref events in the response) and an unpersisted
// marker (retryStateSync) both require the synchronous path.
func TestDestroyLiveAfterPauseStaysSynchronous(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-pause-volumes", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s, _, flow := newDestroyLifecycleServiceForTest(sb)
	restore := stubStoragePluginVolumes(t, func(context.Context, string) (bool, error) { return true, nil })
	defer restore()

	_, err := s.destroyLiveAfterPause(pauseCleanupTestContext(), &cubebox.UpdateCubeSandboxRequest{
		RequestID: "pause-request",
		SandboxID: sb.ID,
	}, sb, false)
	require.NoError(t, err)
	assert.Equal(t, 1, flow.destroyCalls)
	assert.Nil(t, sb.UserMarkDeletedTime)

	sb2 := newCubeboxWithStatusForTest("sb-pause-state-sync", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s2, _, flow2 := newDestroyLifecycleServiceForTest(sb2)
	restore2 := stubStoragePluginVolumes(t, func(context.Context, string) (bool, error) { return false, nil })
	defer restore2()

	_, err = s2.destroyLiveAfterPause(pauseCleanupTestContext(), &cubebox.UpdateCubeSandboxRequest{
		RequestID: "pause-request",
		SandboxID: sb2.ID,
	}, sb2, true)
	require.NoError(t, err)
	assert.Equal(t, 1, flow2.destroyCalls, "retryStateSync must keep the destroy on the response path")
}

// A failed reap keeps the tombstone for the next lifecycle op. Handing it to
// the cleanup flow would reclaim the row and the disk Resume still needs.
func TestPauseCleanupFailureKeepsTombstone(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-reap-fail", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	markPauseCleanupPending(sb, "pause-request")
	s, _, flow := newDestroyLifecycleServiceForTest(sb)
	flow.destroyErr = errors.New("shim did not exit")

	err := s.ensurePauseCleanupFinished(pauseCleanupTestContext(), sb.ID, "resume")
	require.Error(t, err)
	assert.Zero(t, flow.cleanupCreateCalls, "a failed reap must not be handed to the cleanup flow")
	assert.NotNil(t, sb.UserMarkDeletedTime, "the marker is what the next lifecycle op retries")

	// Without the flag the engine still records the failure for the GC.
	plain := &workflow.DestroyContext{
		DestroyInfo:      &cubebox.DestroyCubeSandboxRequest{SandboxID: sb.ID},
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sb.ID},
	}
	_ = s.engine.Destroy(pauseCleanupTestContext(), plain)
	assert.Equal(t, 1, flow.cleanupCreateCalls)
}

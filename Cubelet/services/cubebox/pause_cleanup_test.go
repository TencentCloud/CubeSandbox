// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeletlog "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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

// markPauseCleanupPending is the row state a pause leaves when its teardown
// never reported back.
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

// The pause reply does not wait for the teardown, but it hands it to the reaper.
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

// The reaper waits for the lifecycle lock. A resume that got there first
// already reaped the sandbox, so this run must turn into a no-op.
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

// Plugin volumes produce the volume-ref events carried by the pause response,
// so such destroys must never leave the response path. A marker that could not
// be persisted (retryStateSync) is the other reason to stay synchronous.
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

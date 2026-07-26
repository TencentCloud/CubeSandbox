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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestResumeTaskLockedConvergesSuccessfulResume(t *testing.T) {
	now := time.Now()
	sb := newCubeboxWithStatusForTest("sb-resume-success", cubeboxstore.Status{
		PausedAt: now.Add(-time.Minute).UnixNano(),
	})
	task := &fakeResumeTask{}

	result := resumeTaskLocked(context.Background(), sb, task, resumeOptions{
		taskDeadline:      now.Add(time.Second),
		reconcileDeadline: now.Add(time.Second),
	})

	require.True(t, result.running)
	assert.False(t, result.reconciledRunning)
	assert.Equal(t, errorcode.ErrorCode_Success, result.ret.RetCode)
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausingAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt, "a resumed paused sandbox must be running")
}

func TestResumeTaskLockedContinuesWhenReconciliationProvesRunning(t *testing.T) {
	now := time.Now()
	sb := newCubeboxWithStatusForTest("sb-resume-reconciled", cubeboxstore.Status{
		PausedAt: now.Add(-time.Minute).UnixNano(),
	})
	task := &fakeResumeTask{
		resumeErr: errors.New("ttrpc deadline exceeded"),
		status:    containerd.Running,
	}

	result := resumeTaskLocked(context.Background(), sb, task, resumeOptions{
		taskDeadline:      now.Add(time.Second),
		reconcileDeadline: now.Add(time.Second),
	})

	require.True(t, result.running)
	assert.True(t, result.reconciledRunning)
	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, result.ret.RetCode,
		"explicit Resume preserves its original RPC failure")
	assert.Equal(t, 1, task.statusCalls)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt, "reconciled running state must be usable by normal destroy")
}

func TestResumeTaskLockedStopsWhenRunningCannotBeProven(t *testing.T) {
	now := time.Now()
	sb := newCubeboxWithStatusForTest("sb-resume-still-paused", cubeboxstore.Status{
		PausedAt: now.Add(-time.Minute).UnixNano(),
	})
	task := &fakeResumeTask{
		resumeErr: errors.New("ttrpc deadline exceeded"),
		status:    containerd.Paused,
	}

	result := resumeTaskLocked(context.Background(), sb, task, resumeOptions{
		taskDeadline:      now.Add(time.Second),
		reconcileDeadline: now.Add(time.Second),
	})

	assert.False(t, result.running)
	assert.False(t, result.reconciledRunning)
	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, result.ret.RetCode)
	assert.NotZero(t, sb.GetStatus().Get().PausedAt)
}

func TestReconcileStatusAfterResumeErrorStopsWhenStatusLookupFails(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-resume-status-error", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeResumeTask{statusErr: errors.New("shim status unavailable")}

	running := reconcileStatusAfterResumeError(
		context.Background(), sb, task, errors.New("resume timed out"), time.Now().Add(time.Second))

	assert.False(t, running)
	assert.Equal(t, 1, task.statusCalls)
	assert.NotZero(t, sb.GetStatus().Get().PausedAt)
}

func TestReconcileStatusAfterResumeErrorStopsOnUnexpectedStatus(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-resume-unknown-status", cubeboxstore.Status{
		PausedAt: time.Now().Add(-time.Minute).UnixNano(),
	})
	task := &fakeResumeTask{status: containerd.Unknown}

	running := reconcileStatusAfterResumeError(
		context.Background(), sb, task, errors.New("resume timed out"), time.Now().Add(time.Second))

	assert.False(t, running)
	assert.Equal(t, 1, task.statusCalls)
	assert.NotZero(t, sb.GetStatus().Get().PausedAt)
}

func TestDeleteAutoResumeRejectsPausingSandbox(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-pausing-delete", cubeboxstore.Status{
		PausingAt: time.Now().Add(-time.Second).UnixNano(),
	})

	autoResumed, _, ret := (&service{}).resumePausedSandboxForDestroy(context.Background(), sb)

	require.NotNil(t, ret)
	assert.False(t, autoResumed)
	assert.Equal(t, errorcode.ErrorCode_TaskStateInvalid, ret.RetCode)
	assert.Equal(t, "sandbox is pausing; retry DELETE after 2 seconds", ret.RetMsg)
}

func TestDeleteDeadlineBudget(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	t.Run("no deadline uses the thirty second default", func(t *testing.T) {
		budget, ok := newDeleteDeadlineBudget(context.Background(), now)
		require.True(t, ok)
		assert.Equal(t, 5*time.Second, budget.resume)
		assert.Equal(t, 20*time.Second, budget.cleanup)
		assert.Equal(t, 5*time.Second, budget.response)
		assert.Equal(t, now.Add(5*time.Second), budget.resumeDeadline(now))
		assert.Equal(t, now.Add(25*time.Second), budget.cleanupDeadline())
	})

	t.Run("thirty seconds preserves the current allocation", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(30*time.Second))
		defer cancel()

		budget, ok := newDeleteDeadlineBudget(ctx, now)
		require.True(t, ok)
		assert.Equal(t, 5*time.Second, budget.resume)
		assert.Equal(t, 20*time.Second, budget.cleanup)
		assert.Equal(t, 5*time.Second, budget.response)
	})

	t.Run("short deadline keeps cleanup and response minima", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(10*time.Second))
		defer cancel()

		budget, ok := newDeleteDeadlineBudget(ctx, now)
		require.True(t, ok)
		assert.Equal(t, 10*time.Second-5*time.Second-10*time.Second/6, budget.resume)
		assert.Equal(t, 5*time.Second, budget.cleanup)
		assert.Equal(t, 5*time.Second/3, budget.response)
	})

	t.Run("long deadline caps resume and response reservations", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(60*time.Second))
		defer cancel()

		budget, ok := newDeleteDeadlineBudget(ctx, now)
		require.True(t, ok)
		assert.Equal(t, 5*time.Second, budget.resume)
		assert.Equal(t, 50*time.Second, budget.cleanup)
		assert.Equal(t, 5*time.Second, budget.response)
	})

	t.Run("insufficient deadline rejects the preflight", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(7*time.Second))
		defer cancel()

		_, ok := newDeleteDeadlineBudget(ctx, now)
		assert.False(t, ok)
	})
}

func TestDeleteLifecycleLockDeadlineLeavesResponseTime(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	t.Run("no deadline uses the short lock budget", func(t *testing.T) {
		deadline, ok := deleteLifecycleLockDeadline(context.Background(), now)
		require.True(t, ok)
		assert.Equal(t, now.Add(deleteLifecycleLockMaxWait), deadline)
	})

	t.Run("normal delete deadline caps lock waiting", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(30*time.Second))
		defer cancel()

		deadline, ok := deleteLifecycleLockDeadline(ctx, now)
		require.True(t, ok)
		assert.Equal(t, now.Add(deleteLifecycleLockMaxWait), deadline)
	})

	t.Run("short deadline preserves response time", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(6*time.Second))
		defer cancel()

		deadline, ok := deleteLifecycleLockDeadline(ctx, now)
		require.True(t, ok)
		assert.Equal(t, now.Add(deleteLifecycleLockMaxWait), deadline)
	})

	t.Run("response reserve exhaustion skips lock waiting", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(10*time.Millisecond))
		defer cancel()

		_, ok := deleteLifecycleLockDeadline(ctx, now)
		assert.False(t, ok)
	})
}

func TestDeleteAutoResumeFailurePreservesCapacityDiagnostic(t *testing.T) {
	capacity := &errorcode.Ret{
		RetCode: errorcode.ErrorCode_Conflict,
		RetMsg:  "resume rejected by paused_resource_release_ratio policy: need 1024MB + used 512MB > mem quota 1024MB",
	}

	ret, category := deleteAutoResumeFailure(resumeResult{ret: capacity})

	assert.Same(t, capacity, ret)
	assert.Equal(t, "capacity", category)
}

func TestDeleteAutoResumeFailureReturnsClearRetryableMessage(t *testing.T) {
	ret, category := deleteAutoResumeFailure(resumeResult{ret: &errorcode.Ret{
		RetCode: errorcode.ErrorCode_TaskResumeFailed,
		RetMsg:  "shim did not respond",
	}})

	assert.Equal(t, errorcode.ErrorCode_TaskResumeFailed, ret.RetCode)
	assert.Equal(t, "resume_unavailable", category)
	assert.Equal(t,
		"failed to resume paused sandbox before delete: shim did not respond; retry DELETE after 5 seconds",
		ret.RetMsg)
}

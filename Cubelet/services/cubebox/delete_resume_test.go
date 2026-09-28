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

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
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

func TestDeleteLifecycleLockDeadline(t *testing.T) {
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

	t.Run("expired deadline skips lock waiting", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Millisecond))
		defer cancel()

		_, ok := deleteLifecycleLockDeadline(ctx, now)
		assert.False(t, ok)
	})
}

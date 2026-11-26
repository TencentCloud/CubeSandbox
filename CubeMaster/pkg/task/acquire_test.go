// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapconcurrent"
)

// fakeAcquireHandler lets Acquire fail deterministically and records whether
// Release or HandleTask is ever reached.
type fakeAcquireHandler struct {
	wrapconcurrent.ConcurrentHandle
	acquireErr error
	releases   int32
	handled    int32
}

func (f *fakeAcquireHandler) Acquire(context.Context) error { return f.acquireErr }

func (f *fakeAcquireHandler) Release() { atomic.AddInt32(&f.releases, 1) }

func (f *fakeAcquireHandler) HandleTask(context.Context, *Task) error {
	atomic.AddInt32(&f.handled, 1)
	return nil
}

// TestWorkHandlerAcquireFailureSkipsExecutionAndRelease guards the fix for the
// double-execution / semaphore-corruption bug: when Acquire fails the task must
// be re-queued exactly once, must NOT be executed, and must NOT Release a slot
// it never acquired.
func TestWorkHandlerAcquireFailureSkipsExecutionAndRelease(t *testing.T) {
	h := &fakeAcquireHandler{acquireErr: context.DeadlineExceeded}

	old := l
	l = &localTask{
		handles:      map[OpType]taskHandler{DestroySandbox: h},
		initialDelay: 0, // keep backoffRetryDelay from sleeping
	}
	defer func() { l = old }()

	var queued int32
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(AddAsyncTask, func(*Task) error {
		atomic.AddInt32(&queued, 1)
		return nil
	})

	err := l.workHandler(&Task{TaskType: DestroySandbox, Ctx: context.Background()})

	require.Error(t, err, "acquire failure must surface to the queue worker")
	require.Equal(t, int32(0), atomic.LoadInt32(&h.handled), "task must not run when Acquire failed")
	require.Equal(t, int32(0), atomic.LoadInt32(&h.releases), "Release must not run without a matching Acquire")
	require.Equal(t, int32(1), atomic.LoadInt32(&queued), "task must be re-queued exactly once")
}

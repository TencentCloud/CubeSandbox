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
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestResumeRetryWaitsForFailedAttemptCleanup(t *testing.T) {
	t.Run("fast retry", func(t *testing.T) { testResumeRetryWaitsForFailedAttemptCleanup(t, false) })
	t.Run("slow retry after exhaustion", func(t *testing.T) { testResumeRetryWaitsForFailedAttemptCleanup(t, true) })
}

func testResumeRetryWaitsForFailedAttemptCleanup(t *testing.T, exhausted bool) {
	t.Helper()
	id := "sandbox-resume"
	failed := errors.New("guest agent connection timed out")
	flow := &resumeRollbackTestFlow{
		createErr: failed,
		started:   make(chan *workflow.DestroyContext, 2),
	}
	s, _, _ := newDestroyLifecycleServiceForTest(nil)
	s.config.destroyDeadline = 100 * time.Millisecond
	s.engine.AddCleaupFlow(flow)
	for _, name := range []string{"create", "destroy"} {
		s.engine.AddFlow(name, &workflow.Workflow{
			Name: name, Limiter: semaphore.NewLimiter(1),
			Steps: []*workflow.Step{{Name: flow.ID(), Actions: []workflow.Flow{flow}}},
		})
	}
	ctx := namespaces.WithNamespace(t.Context(), namespaces.Default)
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "first-resume"})
	req := resumeReplaceRequest(id)
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = "snap-original"
	create := &workflow.CreateContext{ReqInfo: req, Failover: true}
	create.ScheduleResumeRollback = func() { s.scheduleResumeRollback(ctx, id, create) }

	unlock := s.sandboxLifecycleLocks.Lock(id)
	err := s.engine.Create(ctx, create)
	pending := s.resumeRollbackPending(id)
	attemptValue, _ := s.resumeRollbacks.Load(id)
	attempt := attemptValue.(*resumeRollback)
	if exhausted {
		// Exercise escalation without waiting for sixty real cleanup deadlines.
		attempt.failures.Store(resumeRollbackFastAttempts - 1)
	}
	unlock()
	require.ErrorIs(t, err, failed)
	require.True(t, pending, "the retry gate must be visible before the first attempt unlocks")

	select {
	case opts := <-flow.started:
		require.True(t, opts.IsResumeRollback)
		require.True(t, opts.IsRollBack)
		require.Equal(t, id, opts.SandboxID)
		require.Empty(t, opts.DestroyInfo.GetAnnotations(), "rollback must leave the pause package alone")
	case <-time.After(time.Second):
		t.Fatal("rollback did not start")
	}
	retry, err := s.Create(ctx, req)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_ResumeRollbackPending, retry.GetRet().GetRetCode())
	list, err := s.List(ctx, &cubeboxpb.ListCubeSandboxRequest{Id: &id})
	require.NoError(t, err)
	require.Empty(t, list.GetItems())

	require.Eventually(t, func() bool {
		return !attempt.busy.Load()
	}, time.Second, time.Millisecond)
	require.True(t, s.resumeRollbackPending(id), "a timed-out rollback must remain retryable")
	if exhausted {
		require.Equal(t, int32(resumeRollbackFastAttempts), attempt.failures.Load())
		require.Greater(t, attempt.nextRetry.Load(), time.Now().Add(9*time.Minute).UnixNano())
		s.retryResumeRollbacks()
		require.False(t, attempt.busy.Load(), "exhausted rollbacks must skip the normal ten-second retry loop")
		require.Empty(t, flow.started)
	} else {
		require.Equal(t, int32(1), attempt.failures.Load())
		require.Zero(t, attempt.nextRetry.Load())
	}

	flow.allowDestroy = true
	attempt.nextRetry.Store(0) // Advance past the slow retry interval.
	s.retryResumeRollbacks()
	require.Eventually(t, func() bool {
		return !s.resumeRollbackPending(id)
	}, time.Second, time.Millisecond)
	require.Zero(t, flow.createFallbackCalls, "generic cleanup must not run while the old runtime could be alive")
}

type resumeRollbackTestFlow struct {
	fakeCreateFlow
	createErr           error
	started             chan *workflow.DestroyContext
	allowDestroy        bool
	createFallbackCalls int
}

func TestResumeRollbackKeepsRuntimeIdentityAfterBundleRemoval(t *testing.T) {
	attempt := &resumeRollback{}
	vmm := utils.ProcessIdentity{Pid: 42, StartTime: 100}
	attempt.rememberRuntime(sandboxRuntimeEvidence{identities: []utils.ProcessIdentity{vmm}})

	// Destroy removed the bundle before the first wait timed out.
	next := attempt.rememberRuntime(sandboxRuntimeEvidence{})
	require.Equal(t, []utils.ProcessIdentity{vmm}, next.identities)
}

func (f *resumeRollbackTestFlow) Create(_ context.Context, opts *workflow.CreateContext) error {
	if opts.ReqInfo == nil {
		f.createFallbackCalls++
		return nil
	}
	opts.SandboxID = resumeFromPauseSandboxID(opts.ReqInfo)
	return f.createErr
}

func (f *resumeRollbackTestFlow) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	f.started <- opts
	if f.allowDestroy {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

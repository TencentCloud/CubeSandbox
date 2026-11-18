// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const (
	resumeRollbackFastAttempts = 60
	resumeRollbackSlowInterval = 10 * time.Minute
)

type resumeRollback struct {
	namespace string
	requestID string
	busy      atomic.Bool
	failures  atomic.Int32
	nextRetry atomic.Int64
	// The shim may remove its bundle before the VMM exits. Keep verified
	// identities across cleanup attempts under the lifecycle lock.
	runtimeIdentities []utils.ProcessIdentity
}

type resumeRollbackContextKey struct{}

func (a *resumeRollback) rememberRuntime(ev sandboxRuntimeEvidence) sandboxRuntimeEvidence {
	for _, id := range ev.identities {
		if !slices.Contains(a.runtimeIdentities, id) {
			a.runtimeIdentities = append(a.runtimeIdentities, id)
		}
	}
	ev.identities = a.runtimeIdentities
	return ev
}

func setResumeRollbackPending(ret *errorcode.Ret) {
	ret.RetCode = errorcode.ErrorCode_ResumeRollbackPending
	ret.RetMsg = "previous resume rollback is in progress; retry after 2 seconds"
}

func (s *service) resumeRollbackPending(id string) bool {
	_, pending := s.resumeRollbacks.Load(id)
	return pending
}

func (s *service) scheduleResumeRollback(ctx context.Context, id string, create *workflow.CreateContext) {
	if create.GetSandboxID() != id {
		// No resources were allocated under the desired ID.
		return
	}
	namespace, _ := namespaces.Namespace(ctx)
	attempt := &resumeRollback{namespace: namespace, requestID: create.ReqInfo.GetRequestID()}
	s.resumeRollbacks.Store(id, attempt)
	s.startResumeRollback(id, attempt)
}

func (s *service) retryResumeRollbacks() {
	s.resumeRollbacks.Range(func(key, value any) bool {
		s.startResumeRollback(key.(string), value.(*resumeRollback))
		return true
	})
}

func (s *service) startResumeRollback(id string, attempt *resumeRollback) {
	if !attempt.busy.CompareAndSwap(false, true) {
		return
	}
	if time.Now().UnixNano() < attempt.nextRetry.Load() {
		attempt.busy.Store(false)
		return
	}
	go func() {
		defer attempt.busy.Store(false)
		defer func() {
			if failure := recover(); failure != nil {
				attempt.recordFailure(context.Background(), id, failure)
			}
		}()

		lockCtx, cancelLock := context.WithTimeout(context.Background(), deleteLifecycleLockMaxWait)
		defer cancelLock()
		unlock, err := s.sandboxLifecycleLocks.LockContext(lockCtx, id)
		if err != nil {
			attempt.recordFailure(context.Background(), id, err)
			return
		}
		defer unlock()
		if current, _ := s.resumeRollbacks.Load(id); current != attempt {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), s.config.destroyDeadline)
		defer cancel()
		ctx = namespaces.WithNamespace(ctx, attempt.namespace)
		ctx = constants.WithRuntimeType(ctx, "io.containerd.cube")
		ctx = constants.WithFailoverOperation(ctx)
		ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{
			Action: "Destroy", CalleeAction: "ResumeRollback", RequestID: attempt.requestID,
			InstanceID: id, Caller: constants.CubeboxServiceID.ID(), Callee: s.engine.ID(),
		})
		opts := &workflow.DestroyContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: id, IsRollBack: true},
			DestroyInfo:      &cubeboxpb.DestroyCubeSandboxRequest{SandboxID: id, RequestID: attempt.requestID},
			IsResumeRollback: true,
		}
		ctx = context.WithValue(ctx, workflow.KDestroyContext, opts)
		ctx = context.WithValue(ctx, resumeRollbackContextKey{}, attempt)
		// Use the workflow directly. The public Destroy path can garbage
		// collect the original pause snapshot, which the next resume needs.
		if err := s.engine.Destroy(ctx, opts); err != nil {
			attempt.recordFailure(ctx, id, err)
			return
		}
		s.resumeRollbacks.Delete(id)
	}()
}

func (a *resumeRollback) recordFailure(ctx context.Context, id string, failure any) {
	count := a.failures.Add(1)
	if count >= resumeRollbackFastAttempts {
		a.nextRetry.Store(time.Now().Add(resumeRollbackSlowInterval).UnixNano())
		log.G(ctx).Errorf("resume rollback for %s failed after %d attempts; operator investigation required; cleanup will retry every %s and resume/delete remain blocked until cleanup succeeds: %v", id, count, resumeRollbackSlowInterval, failure)
		return
	}
	log.G(ctx).Warnf("resume rollback for %s will be retried (attempt %d/%d): %v", id, count, resumeRollbackFastAttempts, failure)
}

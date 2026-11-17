// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type resumeRollback struct {
	namespace string
	requestID string
	busy      atomic.Bool
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
	go func() {
		defer attempt.busy.Store(false)
		defer func() {
			if failure := recover(); failure != nil {
				log.G(context.Background()).Errorf("resume rollback for %s panicked: %v", id, failure)
			}
		}()

		lockCtx, cancelLock := context.WithTimeout(context.Background(), deleteLifecycleLockMaxWait)
		defer cancelLock()
		unlock, err := s.sandboxLifecycleLocks.LockContext(lockCtx, id)
		if err != nil {
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
			log.G(ctx).Warnf("resume rollback for %s will be retried: %v", id, err)
			return
		}
		s.resumeRollbacks.Delete(id)
	}()
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/rollback"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubeboxpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

var rollbackPendingGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cubelet_resume_rollback_pending", Help: "Same-ID resume tasks awaiting completion or cleanup.",
})

func newResumeRollbackCoordinator(ctx context.Context, s *service, dir string) (*rollback.Coordinator, error) {
	return rollback.New(dir, rollback.Options{
		Context: ctx, Timeout: s.config.destroyDeadline, Execute: s.executeResumeRollback,
		OnPending: func(count int) { rollbackPendingGauge.Set(float64(count)) },
		OnFailure: func(r rollback.Record, err error) {
			if r.Failures >= 60 {
				log.G(ctx).Errorf("resume rollback for %s failed %d times; operator investigation required; background retry at %s; DELETE can join or retry cleanup immediately: %v", r.ID, r.Failures, r.NextRetry, err)
			} else {
				log.G(ctx).Warnf("resume rollback for %s will retry after cleanup failure %d: %v", r.ID, r.Failures, err)
			}
		},
		OnDriverError: func(err error) { log.G(ctx).Errorf("resume rollback driver recovered: %v", err) },
	})
}

func (s *service) resumeRollbackPending(id string) bool {
	return s.rollbacks != nil && s.rollbacks.Pending(id)
}

func setResumeRollbackPending(ret *errorcode.Ret) {
	ret.RetCode = errorcode.ErrorCode_ResumeRollbackPending
	ret.RetMsg = "previous resume rollback is in progress; retry after 2 seconds"
}

func (s *service) joinResumeRollback(ctx context.Context, id string) error {
	if deadline, ok := ctx.Deadline(); ok {
		deadline = deadline.Add(-deleteLifecycleResponseReserve)
		if !deadline.After(time.Now()) {
			return context.DeadlineExceeded
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return s.rollbacks.Join(ctx, id)
}

func (s *service) prepareResumeRollback(ctx context.Context, id, requestID string) (*rollback.Task, error) {
	if s.rollbacks == nil {
		return nil, fmt.Errorf("resume rollback coordinator is unavailable")
	}
	// Do not claim cleanup ownership of an existing running sandbox. This
	// check is under the lifecycle lock and precedes the durable create intent.
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, id)
	if err != nil && !errors.Is(err, utils.ErrorKeyNotFound) {
		return nil, err
	}
	if sb != nil {
		status := sb.GetStatus()
		if status == nil || status.Get().State() != cubeboxpb.ContainerState_CONTAINER_PAUSED {
			return nil, ret.Errorf(errorcode.ErrorCode_PreConditionFailed, "sandbox %s already exists and is not paused", id)
		}
	}
	ns, _ := namespaces.Namespace(ctx)
	if ns == "" {
		ns = namespaces.Default
	}
	return s.rollbacks.Prepare(id, ns, requestID)
}

// Schedule implements workflow.RollbackScheduler. A declined task leaves the
// engine's generic failover intact; a prepared same-ID task owns its cleanup.
func (s *service) Schedule(ctx context.Context, create *workflow.CreateContext) bool {
	id := resumeFromPauseSandboxID(create.ReqInfo)
	if id == "" || create.GetSandboxID() != id || !s.resumeRollbackPending(id) {
		return false
	}
	if err := s.rollbacks.Activate(id); err != nil {
		log.G(ctx).Errorf("activate durable resume rollback for %s: %v", id, err)
	}
	return true
}

func (s *service) executeResumeRollback(ctx context.Context, task *rollback.Task) error {
	r := task.Snapshot()
	lockCtx, cancelLock := context.WithTimeout(ctx, deleteLifecycleLockMaxWait)
	defer cancelLock()
	unlock, err := s.sandboxLifecycleLocks.LockContext(lockCtx, r.ID)
	if err != nil {
		return rollback.Defer(err)
	}
	defer unlock()
	if !s.rollbacks.Current(task) {
		return nil
	}
	if s.restarts != nil {
		s.restarts.Stop(r.ID)
	}
	ctx = namespaces.WithNamespace(ctx, r.Namespace)
	ctx = constants.WithRuntimeType(ctx, "io.containerd.cube")
	ctx = constants.WithFailoverOperation(ctx)
	ctx = CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{
		Action: "Destroy", CalleeAction: "ResumeRollback", RequestID: r.RequestID,
		InstanceID: r.ID, Caller: constants.CubeboxServiceID.ID(), Callee: s.engine.ID(),
	})
	opts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: r.ID, IsRollBack: true},
		DestroyInfo:      &cubeboxpb.DestroyCubeSandboxRequest{SandboxID: r.ID, RequestID: r.RequestID},
		Rollback:         task,
	}
	ctx = context.WithValue(ctx, workflow.KDestroyContext, opts)
	// Avoid service.Destroy: Master still owns the original pause package.
	err = s.engine.Destroy(ctx, opts)
	if ret.IsErrorCode(err, errorcode.ErrorCode_ConcurrentFailed) {
		return rollback.Defer(err)
	}
	if cleanupDeferred(err) {
		return rollback.Defer(err)
	}
	return err
}

// Only an aggregate consisting entirely of expected delays is deferred. A
// transient intent must not hide a simultaneous storage or task-delete error.
func cleanupDeferred(err error) bool {
	if err == nil {
		return false
	}
	if many, ok := err.(interface{ WrappedErrors() []error }); ok {
		children := many.WrappedErrors()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cleanupDeferred(child) {
				return false
			}
		}
		return true
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cleanupDeferred(child) {
				return false
			}
		}
		return true
	}
	if expected, ok := err.(interface{ RetryWithoutPenalty() bool }); ok {
		return expected.RetryWithoutPenalty()
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cleanupDeferred(wrapped.Unwrap())
	}
	return false
}

func rollbackJournalDir(s *service) string {
	return filepath.Join(s.cubeboxMgr.config.RootPath, "resume-rollbacks")
}

// collectRollbackEvidence revalidates saved candidates, then durably saves all
// evidence before teardown can delete its original source. It also works when
// a prior partial cleanup has already removed the CubeBox row.
func (l *local) collectRollbackEvidence(ctx context.Context, sb *cubeboxstore.CubeBox, task *rollback.Task) (sandboxRuntimeEvidence, error) {
	r := task.Snapshot()
	if sb == nil {
		sb = &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: r.ID, SandboxID: r.ID}}
	}
	var candidates []runtimePIDCandidate
	// Pin previously verified incarnations before consulting bare PID sources.
	// Otherwise a stale bundle/status PID can adopt an unrelated process that
	// reused the number after the original holder exited.
	for _, p := range r.Evidence.Identities {
		id := utils.ProcessIdentity{Pid: p.PID, StartTime: p.StartTime}
		candidates = append(candidates, runtimePIDCandidate{
			pid: p.PID, source: "the rollback journal", trusted: true, identity: &id,
		})
	}
	for _, p := range r.Evidence.Unresolved {
		c := runtimePIDCandidate{pid: p.PID, source: p.Source, trusted: p.Trusted}
		if p.Identity != nil {
			id := utils.ProcessIdentity{Pid: p.Identity.PID, StartTime: p.Identity.StartTime}
			c.identity = &id
		}
		candidates = append(candidates, c)
	}
	ev := l.collectSandboxRuntimeEvidence(ctx, sb, candidates...)
	if r.Evidence.SpawnIntent != nil && sb.Endpoint.Pid <= 1 {
		// Startup may backfill an intent timestamp in the CubeBox row. The
		// journal's original age (including unknown/zero) remains authoritative.
		ev.unresolved = slices.DeleteFunc(ev.unresolved, func(u unresolvedEvidence) bool { return u.spawnIntent })
		ev.markStaleIntent(*r.Evidence.SpawnIntent, "a shim was spawned but no pid was recorded")
		ev.intentTTL = l.ShimIntentTTL()
		ev.resolveStaleIntents(ctx, r.ID, ev.intentTTL)
	}
	e := rollback.Evidence{SpawnIntent: ev.spawnIntent}
	for _, id := range ev.identities {
		e.Identities = append(e.Identities, rollback.Identity{PID: id.Pid, StartTime: id.StartTime})
	}
	for _, c := range ev.unresolvedPIDs {
		p := rollback.Candidate{PID: c.pid, Source: c.source, Trusted: c.trusted}
		if c.identity != nil {
			p.Identity = &rollback.Identity{PID: c.identity.Pid, StartTime: c.identity.StartTime}
		}
		e.Unresolved = append(e.Unresolved, p)
	}
	merged, err := task.Remember(e)
	if err != nil {
		return ev, fmt.Errorf("persist rollback runtime evidence: %w", err)
	}
	ev.identities = nil
	for _, id := range merged.Identities {
		ev.identities = append(ev.identities, utils.ProcessIdentity{Pid: id.PID, StartTime: id.StartTime})
	}
	return ev, nil
}

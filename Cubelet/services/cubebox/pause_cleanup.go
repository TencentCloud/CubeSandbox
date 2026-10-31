// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// storageSandboxHasPluginVolumes is a var so tests can stub the storage probe.
var storageSandboxHasPluginVolumes = storage.SandboxHasPluginVolumes

// deferPauseCleanup hands the teardown to a background goroutine: detached from
// the pause request's context (the handler cancels its own on return) and
// bounded by destroy_dead_line. Concurrency is bounded by the destroy
// workflow's own limiter, the same one the synchronous path ran under.
func (s *service) deferPauseCleanup(ctx context.Context, sandboxID string) {
	deadline := s.config.destroyDeadline
	if deadline <= 0 {
		deadline = defaultDestroyDeadline
	}
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline)
	go func() {
		defer cancel()
		// A panic keeps the marker; the next lifecycle op retries.
		defer recov.HandleCrash(func(panicError interface{}) {
			log.G(jobCtx).Errorf("pause cleanup panicked: %v", panicError)
		})
		s.reapPausedSandbox(jobCtx, sandboxID)
	}()
}

// pauseCleanupPending is the whole "did the last pause finish?" test: Pause
// marks the tombstone for the duration of its destroy, so a PAUSED row that
// still carries the mark never finished it. A mark on any other state belongs
// to a failed delete/create and is not this path's business.
func (s *service) pauseCleanupPending(ctx context.Context, sandboxID string) bool {
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil || sb == nil {
		return false
	}
	sb.Lock()
	pending := sb.UserMarkDeletedTime != nil
	sb.Unlock()
	st := sb.GetStatus()
	return pending && st != nil && st.Get().State() == cubebox.ContainerState_CONTAINER_PAUSED
}

// reapPausedSandbox finishes a teardown the pause reply left behind. It takes
// the sandbox's lifecycle lock first, so a Resume/Delete that got there before
// it reaped the sandbox itself and this run becomes a no-op.
func (s *service) reapPausedSandbox(ctx context.Context, sandboxID string) {
	unlock, err := s.sandboxLifecycleLocks.LockContext(ctx, sandboxID)
	if err != nil {
		log.G(ctx).Warnf("pause cleanup: %s is busy; its lifecycle operation finishes the teardown", sandboxID)
		return
	}
	defer unlock()
	if !s.pauseCleanupPending(ctx, sandboxID) {
		return
	}
	if err := s.runPauseCleanup(ctx, sandboxID); err != nil {
		log.G(ctx).Errorf("pause cleanup: %s stayed unfinished: %v", sandboxID, err)
	}
}

// ensurePauseCleanupFinished runs the teardown Pause deferred, but only when it
// really did not finish. The caller holds the sandbox's lifecycle lock, so this
// cannot overlap the reaper.
func (s *service) ensurePauseCleanupFinished(ctx context.Context, sandboxID, op string) error {
	if sandboxID == "" || !s.pauseCleanupPending(ctx, sandboxID) {
		return nil
	}
	log.G(ctx).Warnf("%s: %s still carries an unfinished pause cleanup; reaping it now", op, sandboxID)
	return s.runPauseCleanup(ctx, sandboxID)
}

// buildPauseCleanupDestroy is the keep_tombstone destroy the pause path runs,
// either before the reply or on the reaper.
func buildPauseCleanupDestroy(sandboxID, requestID, instanceType string) *workflow.DestroyContext {
	destroyInfo := &workflow.DestroyContext{
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			RequestID: requestID,
			SandboxID: sandboxID,
			Annotations: map[string]string{
				constants.AnnotationPauseKeepTombstone: "true",
			},
		},
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
	}
	if instanceType != "" {
		destroyInfo.DestroyInfo.Annotations[constants.MasterAnnotationInstanceType] = instanceType
	}
	return destroyInfo
}

// runPauseCleanup reaps one sandbox and, on success, clears the marker Pause
// wrote. A failure keeps it so the next Resume/Delete retries.
func (s *service) runPauseCleanup(ctx context.Context, sandboxID string) error {
	start := time.Now()
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil || sb == nil {
		return nil
	}
	sb.Lock()
	requestID, instanceType := sb.DeleteRequestID, sb.InstanceType
	sb.Unlock()

	destroyInfo := buildPauseCleanupDestroy(sandboxID, requestID, instanceType)
	ctx = context.WithValue(ctx, workflow.KDestroyContext, destroyInfo)
	ferr, _ := ret.FromError(s.engine.Destroy(ctx, destroyInfo))
	if !ret.IsSuccessCode(ferr.Code()) {
		log.G(ctx).Errorf("pause cleanup: reaping %s failed after %v: ret=%v msg=%s",
			sandboxID, time.Since(start), ferr.Code(), ferr.Message())
		return fmt.Errorf("pause keep_tombstone destroy ret=%v msg=%s", ferr.Code(), ferr.Message())
	}
	sb.Lock()
	sb.UserMarkDeletedTime = nil
	sb.DeleteRequestID = ""
	sb.Unlock()
	_ = s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sandboxID)
	log.G(ctx).Infof("pause cleanup: reaped %s in %v", sandboxID, time.Since(start))
	return nil
}

// canDeferPauseCleanup keeps the destroy synchronous when it would report
// volume-ref events: those travel in the pause response's ext_info.
func (s *service) canDeferPauseCleanup(ctx context.Context, sandboxID string) bool {
	hasPluginVolumes, err := storageSandboxHasPluginVolumes(ctx, sandboxID)
	if err != nil {
		log.G(ctx).Warnf("pause cleanup: cannot inspect storage of %s (%v); reap synchronously", sandboxID, err)
		return false
	}
	return !hasPluginVolumes
}

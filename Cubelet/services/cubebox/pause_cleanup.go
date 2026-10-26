// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// storageSandboxHasPluginVolumes is a var so tests can stub the storage probe.
var storageSandboxHasPluginVolumes = storage.SandboxHasPluginVolumes

// deferPauseCleanup runs the teardown in the background, detached from the
// pause request's context.
func (s *service) deferPauseCleanup(ctx context.Context, sandboxID string) {
	go func() {
		// A panic keeps the marker; the next lifecycle op retries.
		defer recov.HandleCrash(func(panicError interface{}) {
			log.G(ctx).Errorf("pause cleanup panicked: %v", panicError)
		})
		s.reapPausedSandbox(context.WithoutCancel(ctx), sandboxID)
	}()
}

// pauseCleanupPending reports whether the last pause's teardown never finished:
// a PAUSED row still carrying the tombstone mark. Marks on other states come
// from failed deletes/creates and are not this path's business.
func (s *service) pauseCleanupPending(ctx context.Context, sandboxID string) (bool, error) {
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return false, nil
		}
		return false, err
	}
	if sb == nil {
		return false, nil
	}
	sb.Lock()
	pending := sb.UserMarkDeletedTime != nil
	sb.Unlock()
	st := sb.GetStatus()
	return pending && st != nil && st.Get().State() == cubebox.ContainerState_CONTAINER_PAUSED, nil
}

// reapPausedSandbox finishes an unfinished pause teardown under the lifecycle
// lock; a Resume/Delete that got there first makes this a no-op. The destroy
// deadline starts only after the lock is held, so lock contention does not
// eat into it.
func (s *service) reapPausedSandbox(ctx context.Context, sandboxID string) {
	unlock, err := s.sandboxLifecycleLocks.LockContext(ctx, sandboxID)
	if err != nil {
		log.G(ctx).Warnf("pause cleanup: %s is busy; its lifecycle operation finishes the teardown", sandboxID)
		return
	}
	defer unlock()
	deadline := s.config.destroyDeadline
	if deadline <= 0 {
		deadline = defaultDestroyDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	pending, err := s.pauseCleanupPending(ctx, sandboxID)
	if err != nil {
		log.G(ctx).Warnf("pause cleanup: cannot inspect %s (%v); leaving the marker to the next lifecycle op", sandboxID, err)
		return
	}
	if !pending {
		return
	}
	if err := s.runPauseCleanup(ctx, sandboxID); err != nil {
		log.G(ctx).Errorf("pause cleanup: %s stayed unfinished: %v", sandboxID, err)
	}
}

// ensurePauseCleanupFinished finishes an unfinished pause teardown. The
// caller must hold the sandbox's lifecycle lock.
func (s *service) ensurePauseCleanupFinished(ctx context.Context, sandboxID, op string) error {
	if sandboxID == "" {
		return nil
	}
	pending, err := s.pauseCleanupPending(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("cannot tell whether %s has an unfinished pause cleanup: %w", sandboxID, err)
	}
	if !pending {
		return nil
	}
	log.G(ctx).Warnf("%s: %s still carries an unfinished pause cleanup; reaping it now", op, sandboxID)
	return s.runPauseCleanup(ctx, sandboxID)
}

// buildPauseCleanupDestroy builds the keep_tombstone destroy for a paused
// sandbox.
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

// runPauseCleanup reaps one sandbox and clears the tombstone marker on
// success; a failure keeps it for the next lifecycle op to retry.
func (s *service) runPauseCleanup(ctx context.Context, sandboxID string) error {
	start := time.Now()
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("cannot read %s to reap it: %w", sandboxID, err)
	}
	if sb == nil {
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

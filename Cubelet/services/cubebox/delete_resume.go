// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	// These defaults reproduce the existing 30-second DELETE allocation:
	// 5s resume, 20s cleanup, and 5s response. Shorter deadlines use bounded
	// reservations instead of assuming CubeMaster's configured RPC timeout is
	// always 30 seconds.
	deleteAutoResumeMaxDuration = 5 * time.Second
	deleteAutoResumeMinDuration = time.Second
	deleteResponseReserveMin    = time.Second
	deleteResponseReserveMax    = 5 * time.Second
	deleteCleanupReserveMin     = 5 * time.Second
	deleteCleanupReserveMax     = 20 * time.Second
	deleteLifecycleLockMaxWait  = 2 * time.Second
)

type deleteDeadlineBudget struct {
	deadline time.Time
	resume   time.Duration
	cleanup  time.Duration
	response time.Duration
}

func newDeleteDeadlineBudget(ctx context.Context, now time.Time) (deleteDeadlineBudget, bool) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = now.Add(30 * time.Second)
	}

	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return deleteDeadlineBudget{}, false
	}

	response := deleteResponseReserve(remaining)
	// Reserve one quarter as the minimum cleanup floor. Resume gets the
	// remaining budget up to its cap, so short configured deadlines do not fail
	// solely because the default 20-second cleanup reservation does not fit.
	cleanupFloor := clampDeleteDuration(remaining/4, deleteCleanupReserveMin, deleteCleanupReserveMax)
	availableResume := remaining - response - cleanupFloor
	if availableResume < deleteAutoResumeMinDuration {
		return deleteDeadlineBudget{}, false
	}

	resume := min(availableResume, deleteAutoResumeMaxDuration)
	return deleteDeadlineBudget{
		deadline: deadline,
		resume:   resume,
		cleanup:  remaining - response - resume,
		response: response,
	}, true
}

func clampDeleteDuration(value, minValue, maxValue time.Duration) time.Duration {
	return min(max(value, minValue), maxValue)
}

// deleteResponseReserve gives transport enough time to return from Cubelet.
// One sixth reproduces the existing five-second reserve at the default
// 30-second DELETE deadline, while the bounds keep the reservation useful for
// shorter and longer configured deadlines.
func deleteResponseReserve(remaining time.Duration) time.Duration {
	return clampDeleteDuration(remaining/6, deleteResponseReserveMin, deleteResponseReserveMax)
}

func (b deleteDeadlineBudget) resumeDeadline(now time.Time) time.Time {
	return now.Add(b.resume)
}

func (b deleteDeadlineBudget) cleanupDeadline() time.Time {
	return b.deadline.Add(-b.response)
}

// resumePausedSandboxForDestroy performs the internal preflight needed before
// the normal destroy workflow can safely clean a paused VM. Callers must hold
// the per-sandbox lifecycle lock. autoResumed is true only when this call
// changed a stable PAUSED sandbox to RUNNING.
func (s *service) resumePausedSandboxForDestroy(ctx context.Context, sb *cubeboxstore.CubeBox) (autoResumed bool, budget deleteDeadlineBudget, ret *errorcode.Ret) {
	status := sb.GetStatus()
	if status == nil {
		return false, deleteDeadlineBudget{}, nil
	}

	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"sandboxID": sb.ID,
		"step":      "deleteAutoResume",
	})
	switch status.Get().State() {
	case cubebox.ContainerState_CONTAINER_PAUSING:
		stepLog.WithFields(CubeLog.Fields{
			"outcome":          "rejected",
			"failure_category": "pausing",
			"final_state":      "PAUSING",
		}).Warn("delete auto-resume skipped because sandbox is pausing")
		return false, deleteDeadlineBudget{}, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_TaskStateInvalid,
			RetMsg:  "sandbox is pausing; retry DELETE after 2 seconds",
		}
	case cubebox.ContainerState_CONTAINER_PAUSED:
		// A stable paused sandbox needs a normal runtime before the existing
		// destroy workflow can clean every containerd and VM resource.
	default:
		return false, deleteDeadlineBudget{}, nil
	}

	now := time.Now()
	budget, ok := newDeleteDeadlineBudget(ctx, now)
	if !ok {
		stepLog.WithFields(CubeLog.Fields{
			"outcome":          "rejected",
			"failure_category": "delete_deadline",
			"final_state":      "PAUSED",
		}).Warn("delete auto-resume skipped because the delete deadline budget is insufficient")
		return false, deleteDeadlineBudget{}, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_TaskResumeFailed,
			RetMsg:  "cannot resume paused sandbox before delete: insufficient time remains for resume, cleanup, and response; retry DELETE after 5 seconds",
		}
	}

	stepLog.Infof("delete auto-resume started")
	result := s.resumeLocked(ctx, sb, resumeOptions{
		taskDeadline:      budget.resumeDeadline(now),
		reconcileDeadline: budget.resumeDeadline(now),
		persist:           false,
		skipAdmission:     true, // sandbox will be destroyed immediately after resume
	})
	durationMS := time.Since(now).Milliseconds()
	if result.running {
		// Persist the converged runtime state before normal destroy writes its
		// delete marker. A crash between a successful shim resume and that
		// marker must not leave durable metadata paused while the VM is running.
		if err := s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID); err != nil {
			ret := &errorcode.Ret{
				RetCode: errorcode.ErrorCode_TaskResumeFailed,
				RetMsg:  fmt.Sprintf("failed to persist resumed sandbox before delete: %s; retry DELETE after 5 seconds", err),
			}
			stepLog.WithFields(CubeLog.Fields{
				"duration_ms":      durationMS,
				"outcome":          "failed",
				"failure_category": "persistence",
				"final_state":      "RUNNING",
			}).Warnf("delete auto-resume failed: %s", ret.RetMsg)
			return false, deleteDeadlineBudget{}, ret
		}

		outcome := "resumed"
		if result.reconciledRunning {
			outcome = "reconciled_running"
		}
		stepLog.WithFields(CubeLog.Fields{
			"duration_ms": durationMS,
			"outcome":     outcome,
			"final_state": "RUNNING",
		}).Info("delete auto-resume completed")
		return true, budget, nil
	}

	deleteRet, failureCategory := deleteAutoResumeFailure(result)
	if deleteRet.RetCode == errorcode.ErrorCode_Conflict {
		stepLog.WithFields(CubeLog.Fields{
			"duration_ms":      durationMS,
			"outcome":          "rejected",
			"failure_category": failureCategory,
			"final_state":      "PAUSED",
		}).Warnf("delete auto-resume rejected: %s", deleteRet.RetMsg)
		return false, deleteDeadlineBudget{}, deleteRet
	}

	stepLog.WithFields(CubeLog.Fields{
		"duration_ms":      durationMS,
		"outcome":          "failed",
		"failure_category": failureCategory,
		"final_state":      "UNPROVEN",
	}).Warnf("delete auto-resume failed: %s", deleteRet.RetMsg)
	return false, deleteDeadlineBudget{}, deleteRet
}

func deleteAutoResumeFailure(result resumeResult) (*errorcode.Ret, string) {
	if result.ret != nil && result.ret.RetCode == errorcode.ErrorCode_Conflict {
		return result.ret, "capacity"
	}

	detail := "resume state could not be proven RUNNING"
	if result.ret != nil && result.ret.RetMsg != "" {
		detail = result.ret.RetMsg
	}
	return &errorcode.Ret{
		RetCode: errorcode.ErrorCode_TaskResumeFailed,
		RetMsg:  fmt.Sprintf("failed to resume paused sandbox before delete: %s; retry DELETE after 5 seconds", detail),
	}, "resume_unavailable"
}

func deleteLifecycleLockDeadline(ctx context.Context, now time.Time) (time.Time, bool) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return now.Add(deleteLifecycleLockMaxWait), true
	}

	remaining := deadline.Sub(now)
	response := deleteResponseReserve(remaining)
	latestLockDeadline := deadline.Add(-response)
	if !latestLockDeadline.After(now) {
		return time.Time{}, false
	}
	maxLockDeadline := now.Add(deleteLifecycleLockMaxWait)
	if latestLockDeadline.Before(maxLockDeadline) {
		return latestLockDeadline, true
	}
	return maxLockDeadline, true
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pausesnap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	volrefcount "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/refcount"
)

var (
	destroyOnCubelet         = cubelet.Destroy
	cleanupTemplateOnCubelet = cubelet.CleanupTemplate
	cubeletAddr              = cubelet.GetCubeletAddr
)

// TryDeletePaused handles Delete of a sandbox that has a pause-snapshot binding.
//
// READY: Pause succeeded, shim already reaped. Only the CubeBox tombstone
// remains — delete_tombstone + CleanupTemplate.
//
// FAILED / CREATING: Pause died halfway, shim is still alive. Do not use
// tombstone — full Destroy kills the shim, then CleanupTemplate drops the
// half-finished pause snap.
//
// Returns handled=false when there is no pause binding (caller should Destroy).
func TryDeletePaused(ctx context.Context, requestID, sandboxID, hostIP string) (handled bool, err error) {
	sandboxID = strings.TrimSpace(sandboxID)
	hostIP = strings.TrimSpace(hostIP)
	if sandboxID == "" {
		return false, nil
	}
	rec, err := GetBySandbox(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if rec == nil || strings.TrimSpace(rec.SnapshotID) == "" {
		return false, nil
	}
	if ip := strings.TrimSpace(rec.NodeIP); ip != "" {
		hostIP = ip
	}
	if hostIP == "" {
		return true, fmt.Errorf("pause snapshot %s has no source node for sandbox %s", rec.SnapshotID, sandboxID)
	}

	addr := cubeletAddr(hostIP)
	if isReadyPauseSnapshot(rec.Status) {
		if err := destroyPausedTombstone(ctx, addr, requestID, sandboxID, hostIP); err != nil {
			return true, err
		}
	} else if err := destroyFailedPauseSandbox(ctx, addr, requestID, sandboxID, hostIP); err != nil {
		return true, err
	}

	if err := cleanupPauseSnapshotOnCubelet(ctx, addr, requestID, rec.SnapshotID); err != nil {
		return true, err
	}
	if err := Delete(ctx, rec.SnapshotID); err != nil {
		log.G(ctx).Warnf("delete pause snap meta %s after cleanup: %v", rec.SnapshotID, err)
	}
	return true, nil
}

// isReadyPauseSnapshot is the only case where the shim is already gone.
func isReadyPauseSnapshot(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), statusReady)
}

func destroyPausedTombstone(ctx context.Context, addr, requestID, sandboxID, hostIP string) error {
	destroyRsp, err := destroyOnCubelet(ctx, addr, &cubebox.DestroyCubeSandboxRequest{
		RequestID: requestID,
		SandboxID: sandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationPauseDeleteTombstone: "true",
			constants.CubeAnnotationsInsType:             cubebox.InstanceType_cubebox.String(),
		},
	})
	if err != nil {
		return fmt.Errorf("delete paused tombstone %s on %s: %w", sandboxID, hostIP, err)
	}
	return checkDestroyRet(destroyRsp, sandboxID, "delete paused tombstone")
}

func destroyFailedPauseSandbox(ctx context.Context, addr, requestID, sandboxID, hostIP string) error {
	destroyRsp, err := destroyOnCubelet(ctx, addr, &cubebox.DestroyCubeSandboxRequest{
		RequestID: requestID,
		SandboxID: sandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationsInsType: cubebox.InstanceType_cubebox.String(),
		},
	})
	if err != nil {
		return fmt.Errorf("destroy failed-pause sandbox %s on %s: %w", sandboxID, hostIP, err)
	}
	if err := checkDestroyRet(destroyRsp, sandboxID, "destroy failed-pause sandbox"); err != nil {
		return err
	}
	if destroyRsp != nil {
		volrefcount.ApplyFromExtInfo(ctx, destroyRsp.GetExtInfo())
	}
	return nil
}

func checkDestroyRet(destroyRsp *cubebox.DestroyCubeSandboxResponse, sandboxID, what string) error {
	if destroyRsp == nil || destroyRsp.GetRet() == nil {
		return nil
	}
	code := destroyRsp.GetRet().GetRetCode()
	if code == cubeleterrorcode.ErrorCode_Success || code == cubeleterrorcode.ErrorCode_OK {
		return nil
	}
	msg := destroyRsp.GetRet().GetRetMsg()
	if strings.Contains(strings.ToLower(msg), "not found") {
		return nil
	}
	return fmt.Errorf("%s %s ret=%v msg=%s", what, sandboxID, code, msg)
}

func cleanupPauseSnapshotOnCubelet(ctx context.Context, addr, requestID, snapshotID string) error {
	rsp, err := cleanupTemplateOnCubelet(ctx, addr, &cubebox.CleanupTemplateRequest{
		RequestID:  requestID,
		TemplateID: snapshotID,
	})
	if err != nil {
		return fmt.Errorf("cleanup pause snap %s: %w", snapshotID, err)
	}
	if rsp == nil || rsp.GetRet() == nil {
		return fmt.Errorf("nil CleanupTemplate response for pause snap %s", snapshotID)
	}
	code := rsp.GetRet().GetRetCode()
	if code != cubeleterrorcode.ErrorCode_Success && code != cubeleterrorcode.ErrorCode_OK {
		return fmt.Errorf("cleanup pause snap %s ret=%v msg=%s",
			snapshotID, code, rsp.GetRet().GetRetMsg())
	}
	return nil
}

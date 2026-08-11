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
)

// TryDeletePaused handles Delete of a paused sandbox (no live runtime):
//  1. Cubelet Destroy(delete_tombstone) → remove PAUSED CubeBox List row
//  2. CleanupTemplate → remove pause snap CoW/catalog
//  3. pausesnap.Delete → remove Master binding
//
// Volume refcounts were already adjusted on Pause (same as Destroy).
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

	addr := cubelet.GetCubeletAddr(hostIP)

	// Remove PAUSED CubeBox so List no longer shows this sandbox.
	destroyRsp, err := cubelet.Destroy(ctx, addr, &cubebox.DestroyCubeSandboxRequest{
		RequestID: requestID,
		SandboxID: sandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationPauseSkipAutoResume:  "true",
			constants.CubeAnnotationPauseDeleteTombstone: "true",
			constants.CubeAnnotationsInsType:             cubebox.InstanceType_cubebox.String(),
		},
	})
	if err != nil {
		return true, fmt.Errorf("delete paused tombstone %s on %s: %w", sandboxID, hostIP, err)
	}
	if destroyRsp != nil && destroyRsp.GetRet() != nil {
		code := destroyRsp.GetRet().GetRetCode()
		if code != cubeleterrorcode.ErrorCode_Success && code != cubeleterrorcode.ErrorCode_OK {
			// Idempotent if tombstone already gone (Destroy treats missing as success).
			msg := destroyRsp.GetRet().GetRetMsg()
			if !strings.Contains(strings.ToLower(msg), "not found") {
				return true, fmt.Errorf("delete paused tombstone %s ret=%v msg=%s", sandboxID, code, msg)
			}
		}
	}

	rsp, err := cubelet.CleanupTemplate(ctx, addr, &cubebox.CleanupTemplateRequest{
		RequestID:  requestID,
		TemplateID: rec.SnapshotID,
	})
	if err != nil {
		return true, fmt.Errorf("cleanup pause snap %s on %s: %w", rec.SnapshotID, hostIP, err)
	}
	if rsp == nil || rsp.GetRet() == nil {
		return true, fmt.Errorf("nil CleanupTemplate response for pause snap %s", rec.SnapshotID)
	}
	code := rsp.GetRet().GetRetCode()
	if code != cubeleterrorcode.ErrorCode_Success && code != cubeleterrorcode.ErrorCode_OK {
		return true, fmt.Errorf("cleanup pause snap %s ret=%v msg=%s",
			rec.SnapshotID, code, rsp.GetRet().GetRetMsg())
	}

	if err := Delete(ctx, rec.SnapshotID); err != nil {
		log.G(ctx).Warnf("delete pause snap meta %s after cleanup: %v", rec.SnapshotID, err)
	}
	return true, nil
}

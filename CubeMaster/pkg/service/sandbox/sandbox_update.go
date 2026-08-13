// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func Update(ctx context.Context, req *types.UpdateRequest) (rsp *types.Res) {
	rsp = &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		logger := log.G(ctx).WithFields(map[string]interface{}{
			"RequestId": req.RequestID,
			"RetCode":   int64(rsp.Ret.RetCode),
		})
		logger.Infof("Update:%+v", utils.InterfaceToString(req))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("Update fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	if req.SandboxID == "" || req.InstanceType == "" || req.Action == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide InstanceType,SandboxID,Action"
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "action should be pause or resume"
		return
	}
	if req.Action == "resume" && req.Timeout != nil && *req.Timeout < types.NeverTimeout {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "timeout must be >= -1 (use -1 for never timeout)"
		return
	}
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}

	// Covers API pause/resume and CLM auto-pause/auto-resume (both hit Update).
	// Hold until pause/resume reaches a terminal Master outcome; concurrent
	// delete/resume/pause on the same sandboxID must wait or Conflict.
	lockOpts := sandboxlock.Options{Value: req.Action}
	switch req.Action {
	case "pause":
		lockOpts.TTL = sandboxlock.PauseTTL
	case "resume":
		lockOpts.TTL = sandboxlock.ResumeTTL
	}
	err := sandboxlock.WithLock(ctx, req.SandboxID, lockOpts, func(ctx context.Context) error {
		// Client/HTTP cancel must not abort bookkeeping or release the lock
		// while SAVE/Create/Destroy is still in flight on Cubelet.
		ctx = context.WithoutCancel(ctx)
		var hostIP string
		if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
			hostIP = v.HostIP
		} else if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID); ok {
			hostIP = proxyMap.HostIP
		} else if ip, ok := resolvePauseHostIP(ctx, req.SandboxID); ok {
			hostIP = ip
		} else {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = "sandbox not found"
			return nil
		}
		if config.GetConfig().Common.MockUpdateAction {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_Success)
			rsp.Ret.RetMsg = "mock update action success"
			return nil
		}

		switch req.Action {
		case "pause":
			*rsp = *pauseSandbox(ctx, req, hostIP)
		case "resume":
			*rsp = *resumeFromPauseSnapshot(ctx, req, hostIP)
		}
		return nil
	})
	if err != nil {
		applyLifecycleLockError(rsp, err)
	}
	return
}

// publishUpdateTimeout starts a new lifecycle timeout window after a successful
// sandbox resume. A non-zero Timeout replaces the stored timeout; nil or 0
// preserves the stored timeout while moving its CreatedAt and EndAt forward
// from the resume time. Metadata updates are best effort and never change the
// resume response.
func publishUpdateTimeout(ctx context.Context, req *types.UpdateRequest) {
	if req == nil || req.Action != "resume" {
		return
	}
	if req.Timeout == nil || *req.Timeout == 0 {
		if p := getTimeoutProvider(); p != nil {
			if _, err := p.RebaseTimeoutWindow(ctx, req.SandboxID); err != nil {
				log.G(ctx).Warnf("lifecycle: RebaseTimeoutWindow sandbox=%s failed: %v", req.SandboxID, err)
			}
		}
		return
	}
	// refreshTimeoutMeta updates lifecycle metadata through the timeout provider.
	// Resume does not return endAt, so the computed value is intentionally ignored.
	refreshTimeoutMeta(ctx, req.SandboxID, *req.Timeout)
}

func applyLifecycleLockError(rsp *types.Res, err error) {
	if rsp == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, sandboxlock.ErrLockNotAcquired):
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Conflict)
		rsp.Ret.RetMsg = "sandbox lifecycle operation in progress; retry later"
	case errors.Is(err, sandboxlock.ErrRedisUnavailable):
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
	default:
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
	}
}

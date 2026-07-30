// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// SetTimeout implements POST /cube/sandbox/timeout. It is the master-side
// counterpart of CubeAPI's SetTimeoutRequest -> SetTimeoutResponse.
func SetTimeout(ctx context.Context, req *types.SetTimeoutRequest) (rsp *types.SetTimeoutRes) {
	rsp = &types.SetTimeoutRes{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
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
		logger.Infof("SetTimeout:%+v", utils.InterfaceToString(req))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("SetTimeout fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	if req.SandboxID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide sandboxID"
		return
	}
	if req.Timeout < -1 {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "timeout must be >= -1 (use -1 for never timeout)"
		return
	}
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}

	if !sandboxExists(ctx, req.SandboxID) {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_NotFound)
		rsp.Ret.RetMsg = "sandbox not found"
		return
	}

	endAt := refreshTimeoutMeta(ctx, req.SandboxID, int(req.Timeout))
	rsp.EndAt = endAt
	return
}

// Refresh implements POST /cube/sandbox/refresh. Omitted duration falls back to
// the cluster default timeout. Explicit duration accepts -1 for never-timeout
// or a positive TTL; 0 is rejected because immediate timeout belongs to
// SetTimeout.
func Refresh(ctx context.Context, req *types.RefreshSandboxRequest) (rsp *types.RefreshSandboxRes) {
	rsp = &types.RefreshSandboxRes{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
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
		logger.Infof("RefreshSandbox:%+v", utils.InterfaceToString(req))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("RefreshSandbox fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	if req.SandboxID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide sandboxID"
		return
	}
	var duration int
	if req.Duration == nil {
		duration, _ = resolveTimeoutSeconds(nil, config.GetConfig().CubeletConf.DefaultTimeoutInsec)
	} else {
		duration = int(*req.Duration)
	}
	if duration == 0 || duration < types.NeverTimeout {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "duration must be -1 or positive"
		return
	}
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}

	if !sandboxExists(ctx, req.SandboxID) {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_NotFound)
		rsp.Ret.RetMsg = "sandbox not found"
		return
	}

	endAt := refreshTimeoutMeta(ctx, req.SandboxID, duration)
	rsp.EndAt = endAt
	return
}

func sandboxExists(ctx context.Context, sandboxID string) bool {
	if v := localcache.GetSandboxCache(sandboxID); v != nil {
		return true
	}
	if _, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok {
		return true
	}
	return false
}

// refreshTimeoutMeta updates the lifecycle timeout record for sandboxID and
// returns its new endAt as a Unix millisecond timestamp.
//
// When a provider is installed, RefreshTimeout must be called for every valid
// timeout, including NeverTimeout (-1), because it both stores the new timeout
// and publishes the lifecycle update. For a finite timeout, this function uses
// the provider's positive endAt. For NeverTimeout, it ignores the provider's
// endAt and returns 0 because -1 means that no deadline exists and 0 is the
// canonical endAt representation. If the provider is unavailable, fails, or
// returns no positive endAt, a finite timeout falls back to now() + timeout.
func refreshTimeoutMeta(ctx context.Context, sandboxID string, timeoutSeconds int) int64 {
	if p := getTimeoutProvider(); p != nil {
		endAt, err := p.RefreshTimeout(ctx, sandboxID, timeoutSeconds)
		if err != nil {
			log.G(ctx).Warnf("lifecycle: RefreshTimeout sandbox=%s failed: %v", sandboxID, err)
		} else if (timeoutSeconds != types.NeverTimeout) && endAt > 0 {
			return endAt
		}
	}
	// All callers reject values below NeverTimeout before reaching this helper.
	if timeoutSeconds == types.NeverTimeout {
		return 0
	}
	return time.Now().UnixMilli() + int64(timeoutSeconds)*1000
}

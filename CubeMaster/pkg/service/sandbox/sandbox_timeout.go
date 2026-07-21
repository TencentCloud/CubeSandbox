// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"time"

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
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}
	if req.Timeout < -1 {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "timeout must be >= -1 (use -1 for never timeout)"
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

// Refresh implements POST /cube/sandbox/refresh. Semantically identical to
// SetTimeout: refresh(d) rebases the idle clock and sets TimeoutSeconds = d.
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
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}
	if req.Duration <= 0 {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "duration must be positive (seconds)"
		return
	}

	if !sandboxExists(ctx, req.SandboxID) {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_NotFound)
		rsp.Ret.RetMsg = "sandbox not found"
		return
	}

	endAt := refreshTimeoutMeta(ctx, req.SandboxID, int(req.Duration))
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

func refreshTimeoutMeta(ctx context.Context, sandboxID string, timeoutSeconds int) int64 {
	if p := getTimeoutProvider(); p != nil {
		endAt, err := p.RefreshTimeout(ctx, sandboxID, timeoutSeconds)
		if err != nil {
			log.G(ctx).Warnf("lifecycle: RefreshTimeout sandbox=%s failed: %v", sandboxID, err)
		} else if endAt > 0 {
			return endAt
		}
	}
	return time.Now().UnixMilli() + int64(timeoutSeconds)*1000
}

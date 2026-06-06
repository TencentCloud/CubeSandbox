// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strconv"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// SetTimeout sets the absolute TTL of a running sandbox: the reaper will
// destroy the sandbox once `now + timeout` is reached.
//
// Semantics (matches CubeAPI POST /sandboxes/{id}/timeout):
//   - timeout >  0  → schedule destroy at now+timeout seconds
//   - timeout == 0  → cancel any TTL (sandbox lives until killed explicitly)
//   - timeout <  0  → 400 ParamsError
func SetTimeout(ctx context.Context, req *types.SandboxTimeoutRequest) *types.SandboxTimeoutResponse {
	rsp := &types.SandboxTimeoutResponse{RequestID: req.RequestID, SandboxID: req.SandboxID}
	log.G(ctx).Infof("SetTimeout req:%s", utils.InterfaceToString(req))
	rsp.Ret, rsp.EndAt = applyAbsoluteTTL(ctx, req.SandboxID, req.Timeout)
	if rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		log.G(ctx).Infof("SetTimeout ok sandbox=%s timeout=%d end_at=%s",
			req.SandboxID, req.Timeout, rsp.EndAt)
	} else {
		log.G(ctx).Errorf("SetTimeout fail:%s rsp:%s",
			utils.InterfaceToString(req), utils.InterfaceToString(rsp))
	}
	return rsp
}

// Refresh sets the absolute TTL of a running sandbox to `now + duration`.
func Refresh(ctx context.Context, req *types.SandboxRefreshRequest) *types.SandboxRefreshResponse {
	rsp := &types.SandboxRefreshResponse{RequestID: req.RequestID, SandboxID: req.SandboxID}
	log.G(ctx).Infof("Refresh req:%s", utils.InterfaceToString(req))
	rsp.Ret, rsp.EndAt = applyAbsoluteTTL(ctx, req.SandboxID, req.Duration)
	if rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		log.G(ctx).Infof("Refresh ok sandbox=%s duration=%d end_at=%s",
			req.SandboxID, req.Duration, rsp.EndAt)
	} else {
		log.G(ctx).Errorf("Refresh fail:%s rsp:%s",
			utils.InterfaceToString(req), utils.InterfaceToString(rsp))
	}
	return rsp
}

func applyAbsoluteTTL(ctx context.Context, sandboxID string, seconds int) (*types.Ret, string) {
	if sandboxID == "" {
		return paramsError("sandbox_id is empty"), ""
	}
	if seconds < 0 {
		return paramsError("timeout must be >= 0"), ""
	}
	proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID)
	if !ok || proxy == nil {
		return &types.Ret{
			RetCode: int(errorcode.ErrorCode_NotFound),
			RetMsg:  "sandbox not found",
		}, ""
	}

	var endAtRFC string
	if seconds == 0 {
		proxy.EndAt = ""
	} else {
		endAt := time.Now().Add(time.Duration(seconds) * time.Second)
		proxy.EndAt = strconv.FormatInt(endAt.UnixNano(), 10)
		endAtRFC = endAt.UTC().Format(time.RFC3339Nano)
	}

	if err := localcache.SetSandboxProxyMap(ctx, proxy); err != nil {
		return &types.Ret{
			RetCode: int(errorcode.ErrorCode_DBError),
			RetMsg:  err.Error(),
		}, ""
	}
	return &types.Ret{
		RetCode: int(errorcode.ErrorCode_Success),
		RetMsg:  errorcode.ErrorCode_Success.String(),
	}, endAtRFC
}

func paramsError(msg string) *types.Ret {
	return &types.Ret{
		RetCode: int(errorcode.ErrorCode_MasterParamsError),
		RetMsg:  msg,
	}
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
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
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}
	if req.Action == "resume" && req.Timeout != nil && *req.Timeout < types.NeverTimeout {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "timeout must be >= -1 (use -1 for never timeout)"
		return
	}

	var hostIP string
	if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
		hostIP = v.HostIP
	} else if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID); ok {
		hostIP = proxyMap.HostIP
	} else {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "sandbox not found"
		return
	}
	if config.GetConfig().Common.MockUpdateAction {
		publishUpdateTimeout(ctx, req)
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Success)
		rsp.Ret.RetMsg = "mock update action success"
		return
	}
	calleeEndpoint := cubelet.GetCubeletAddr(hostIP)

	cubeletReq := &cubebox.UpdateCubeSandboxRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationsUpdateAction: req.Action,
			constants.CubeAnnotationsInsType:      req.InstanceType,
		},
	}
	cubeRsp, err := cubelet.Update(ctx, calleeEndpoint, cubeletReq)
	if err != nil || cubeRsp == nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		if err != nil {
			rsp.Ret.RetMsg = err.Error()
		} else {
			rsp.Ret.RetMsg = "cubelet response is nil"
		}
		return
	}
	if cubeRsp.GetRet() == nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Unknown)
		rsp.Ret.RetMsg = "cubelet response ret is nil"
		return
	}
	rsp.Ret.RetCode = int(cubeRsp.GetRet().GetRetCode())
	rsp.Ret.RetMsg = cubeRsp.GetRet().GetRetMsg()
	if rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		publishUpdateTimeout(ctx, req)
		// Only on genuine success — IsAlreadyInState / NotFound are handled
		// upstream by CLM's own reconciliation and would send misleading
		// state signals through the lifecycle channel.
		runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, req.Action, req.RequestID)
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

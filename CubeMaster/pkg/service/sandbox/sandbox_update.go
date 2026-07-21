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
		// Only on genuine success — IsAlreadyInState / NotFound are handled
		// upstream by CLM's own reconciliation and would send misleading
		// state signals through the lifecycle channel.
		runAfterUpdateSandboxSuccessHook(ctx, req.SandboxID, req.InstanceType, req.Action, req.RequestID)
	}
	return
}

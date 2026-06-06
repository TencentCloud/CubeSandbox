// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strconv"
	"time"

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

	// On a successful resume, refresh the absolute deadline so the
	// TTL reaper aligns with the new lifetime requested by the caller.
	// Pause does not touch EndAt; a later resume will rewrite it.
	if req.Action == "resume" && req.Timeout > 0 &&
		rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		refreshSandboxEndAt(ctx, req.SandboxID, time.Duration(req.Timeout)*time.Second)
	}
	return
}

func refreshSandboxEndAt(ctx context.Context, sandboxID string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID)
	if !ok || proxy == nil {
		log.G(ctx).Warnf("refreshSandboxEndAt: proxy map missing for %s", sandboxID)
		return
	}
	proxy.EndAt = strconv.FormatInt(time.Now().Add(ttl).UnixNano(), 10)
	if err := localcache.SetSandboxProxyMap(ctx, proxy); err != nil {
		log.G(ctx).Warnf("refreshSandboxEndAt: rewrite proxy for %s failed: %v", sandboxID, err)
	}
}

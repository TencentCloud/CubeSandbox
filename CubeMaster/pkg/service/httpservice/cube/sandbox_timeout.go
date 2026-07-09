// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleSandboxTimeoutAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodPost {
		return &types.SetTimeoutRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}

	req := &types.SetTimeoutRequest{}
	if err := utils.DecodeHttpBody(r.Body, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &types.SetTimeoutRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.RequestID = req.RequestID
	rt.InstanceID = req.SandboxID
	rt.InstanceType = req.InstanceType

	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]interface{}{
		"RequestId":    req.RequestID,
		"InstanceId":   req.SandboxID,
		"InstanceType": req.InstanceType,
	}))
	res := sandbox.SetTimeout(CubeLog.WithRequestTrace(ctx, rt), req)
	if res != nil && res.Ret != nil {
		rt.RetCode = int64(res.Ret.RetCode)
	}
	return res
}

func handleSandboxRefreshAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodPost {
		return &types.RefreshSandboxRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}

	req := &types.RefreshSandboxRequest{}
	if err := utils.DecodeHttpBody(r.Body, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &types.RefreshSandboxRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.RequestID = req.RequestID
	rt.InstanceID = req.SandboxID
	rt.InstanceType = req.InstanceType

	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]interface{}{
		"RequestId":    req.RequestID,
		"InstanceId":   req.SandboxID,
		"InstanceType": req.InstanceType,
	}))
	res := sandbox.Refresh(CubeLog.WithRequestTrace(ctx, rt), req)
	if res != nil && res.Ret != nil {
		rt.RetCode = int64(res.Ret.RetCode)
	}
	return res
}

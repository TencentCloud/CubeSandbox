// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleUpdateAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	rt.RetCode = -1
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: -1,
			RetMsg:  http.StatusText(http.StatusNotFound),
		},
	}
	req := &types.UpdateRequest{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "请求体解析失败"
		common.WriteAPI(c, rsp)
		return
	}
	if req.RequestID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "requestID is empty"
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, rsp)
		return
	}
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.RequestID = req.RequestID
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	rsp = sandbox.Update(CubeLog.WithRequestTrace(ctx, rt), req)
	common.WriteAPI(c, rsp)
}

// handleSandboxNetworkAction serves POST /cube/sandbox/network, which replaces
// the egress policy of a running sandbox.
func handleSandboxNetworkAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())

	req := &types.UpdateNetworkRequest{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &types.UpdateNetworkRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
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

	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]interface{}{
		"RequestId":    req.RequestID,
		"InstanceId":   req.SandboxID,
		"InstanceType": req.InstanceType,
	}))
	res := sandbox.UpdateNetwork(CubeLog.WithRequestTrace(ctx, rt), req)
	if res != nil && res.Ret != nil {
		rt.RetCode = int64(res.Ret.RetCode)
	}
	common.WriteAPI(c, res)
}

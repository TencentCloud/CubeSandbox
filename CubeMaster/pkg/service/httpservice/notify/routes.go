// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package notify

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func RegisterNotifyRoutes(g *gin.RouterGroup) {
	g.POST(HostChangeNotifyAction, hostChangeGinHandler)
	g.GET(HealthCheckAction, healthCheckGinHandler)
}

func hostChangeGinHandler(c *gin.Context) {
	ctx := c.Request.Context()
	rt := CubeLog.GetTraceInfo(ctx)
	req := &types.HostChangeEvent{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RequestID = req.RequestID
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]any{
		"RequestId": req.RequestID,
	}))
	rsp := hostChangeNotify(ctx, req)
	rt.RetCode = int64(rsp.Ret.RetCode)
	common.WriteResponse(c.Writer, http.StatusOK, rsp)
}

func healthCheckGinHandler(c *gin.Context) {
	rsp := healthCheck(c.Writer, c.Request)
	common.WriteResponse(c.Writer, http.StatusOK, rsp)
}

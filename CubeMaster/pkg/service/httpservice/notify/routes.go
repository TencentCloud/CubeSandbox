// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package notify

import (
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
		common.WriteAPI(c, &types.Res{
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
	common.WriteAPI(c, rsp)
}

func healthCheckGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	rsp := healthCheck(c.Request)
	rt.RetCode = int64(rsp.Ret.RetCode)
	common.WriteAPI(c, rsp)
}

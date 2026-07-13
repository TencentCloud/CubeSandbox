// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func RegisterInnerRoutes(g *gin.RouterGroup) {
	g.GET(NodeAction, nodeGinHandler)
	g.Any(StateWs, websocketGinHandler)
	g.Any(StateQuery, queryGinHandler)
}

func nodeGinHandler(c *gin.Context) {
	ctx := c.Request.Context()
	rt := CubeLog.GetTraceInfo(ctx)
	req := &types.GetNodeReq{}
	querys := c.Request.URL.Query()
	req.RequestID = querys.Get("requestID")
	req.HostID = querys.Get("host_id")
	if ss := querys.Get("score_only"); ss == "true" {
		req.ScoreOnly = true
	}
	rt.RequestID = req.RequestID
	rsp := getNodeInfo(ctx, req)
	common.WriteResponse(c.Writer, http.StatusOK, rsp)
}

func websocketGinHandler(c *gin.Context) {
	handleWebsocket(c.Writer, c.Request)
}

func queryGinHandler(c *gin.Context) {
	handleQuery(c.Writer, c.Request)
}

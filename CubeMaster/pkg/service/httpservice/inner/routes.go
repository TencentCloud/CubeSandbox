// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func RegisterInnerRoutes(g *gin.RouterGroup) {
	g.GET(NodeAction, nodeGinHandler)
	g.Any(StateWs, websocketGinHandler)
	g.Any(StateQuery, queryGinHandler)
	g.POST(FakeCreateAction, fakeCreateGinHandler)
}

// fakeCreateGinHandler preserves the legacy POST /internal/fake_create route.
// The old mux inner.HttpHandler had no case for it, so it fell through to the
// default branch → HTTP 200 + {ret:{ret_code:-1, ret_msg:"Not Found"}} (and ran
// the auth middleware, like any registered route). Kept for backward
// compatibility with probes/scripts that expect the path to exist.
func fakeCreateGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	rt.RetCode = -1
	common.WriteAPI(c, &types.Res{Ret: &types.Ret{RetCode: -1, RetMsg: "Not Found"}})
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
	common.WriteAPI(c, rsp)
}

func websocketGinHandler(c *gin.Context) {
	handleWebsocket(c.Writer, c.Request)
}

func queryGinHandler(c *gin.Context) {
	handleQuery(c.Writer, c.Request)
}

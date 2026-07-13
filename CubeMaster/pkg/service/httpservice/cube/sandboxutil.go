// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleSandboxAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	var res interface{}
	switch c.Request.Method {
	case http.MethodPost:
		res = createSandbox(c.Writer, c.Request, rt)
	case http.MethodDelete:
		res = deleteSandbox(c.Writer, c.Request, rt)
	default:
		res = &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
	common.WriteResponse(c.Writer, http.StatusOK, res)
}

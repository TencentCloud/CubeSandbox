// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"io"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleInfoAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &types.GetCubeSandboxReq{}

	err := utils.DecodeHttpBody(c.Request.Body, req)
	if err != nil {
		if errors.Is(err, io.EOF) {
			req.RequestID = c.Query("requestID")
			req.HostID = c.Query("host_id")
			req.SandboxID = c.Query("sandbox_id")
			req.InstanceType = c.Query("instance_type")
			if containerPort := c.Query("container_port"); containerPort != "" {
				port, _ := strconv.ParseInt(containerPort, 10, 32)
				req.ContainerPort = int32(port)
			}
		} else {
			rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
			common.WriteAPI(c, &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				},
			})
			return
		}
	}
	rt.RequestID = req.RequestID
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	rsp := sandbox.SandboxInfo(ctx, req)
	rt.RetCode = int64(rsp.Ret.RetCode)
	common.WriteAPI(c, rsp)
}

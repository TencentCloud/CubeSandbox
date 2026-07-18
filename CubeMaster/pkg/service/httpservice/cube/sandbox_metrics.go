// Copyright (c) 2026 Tencent Inc.
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
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

// handleSandboxMetricsAction accepts both CubeAPI's GET query form and the
// existing CubeMaster JSON body form, then forwards the normalized request into
// the sandbox service layer.
func handleSandboxMetricsAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &types.GetSandboxMetricsReq{}
	err := utils.DecodeHttpBody(c.Request.Body, req)
	if err != nil {
		if errors.Is(err, io.EOF) {
			req.RequestID = c.Query("requestID")
			req.HostID = c.Query("host_id")
			req.SandboxID = c.Query("sandbox_id")
			req.InstanceType = c.Query("instance_type")
			if start := c.Query("start"); start != "" {
				v, parseErr := strconv.ParseInt(start, 10, 64)
				if parseErr != nil {
					rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
					common.WriteAPI(c, metricsParamError(parseErr.Error()))
					return
				}
				req.Start = v
			}
			if end := c.Query("end"); end != "" {
				v, parseErr := strconv.ParseInt(end, 10, 64)
				if parseErr != nil {
					rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
					common.WriteAPI(c, metricsParamError(parseErr.Error()))
					return
				}
				req.End = v
			}
		} else {
			rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
			common.WriteAPI(c, metricsParamError(err.Error()))
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
	rsp := sandbox.SandboxMetrics(ctx, req)
	rt.RetCode = int64(rsp.Ret.RetCode)
	common.WriteAPI(c, rsp)
}

func metricsParamError(msg string) *types.GetSandboxMetricsRes {
	return &types.GetSandboxMetricsRes{
		Data: []*types.SandboxMetricData{},
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  msg,
		},
	}
}

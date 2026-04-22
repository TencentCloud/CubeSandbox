// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleInfoAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &types.GetCubeSandboxReq{}

	err := utils.DecodeHttpBody(r.Body, req)
	if err != nil {
		if errors.Is(err, io.EOF) {
			queries := r.URL.Query()
			req.RequestID = queries.Get("requestID")
			req.HostID = queries.Get("host_id")
			req.SandboxID = queries.Get("sandbox_id")
			req.InstanceType = queries.Get("instance_type")
			if containerPort := queries.Get("container_port"); containerPort != "" {
				port, _ := strconv.ParseInt(containerPort, 10, 32)
				req.ContainerPort = int32(port)
			}
		} else {
			rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
			return &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				},
			}
		}
	}
	rt.RequestID = req.RequestID
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	rsp := sandbox.SandboxInfo(ctx, req)
	rt.RetCode = int64(rsp.Ret.RetCode)
	return rsp
}

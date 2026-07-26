// Copyright (c) 2024 Tencent Inc.
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

func deleteSandbox(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &types.DeleteCubeSandboxReq{}
	if err := utils.DecodeHttpBody(r.Body, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &types.Res{
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
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]interface{}{
		"RequestId":    req.RequestID,
		"InstanceId":   req.SandboxID,
		"InstanceType": req.InstanceType,
	}))
	ret := sandbox.DestroySandbox(ctx, req)
	rt.RetCode = int64(ret.Ret.RetCode)
	return ret
}

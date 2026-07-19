// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"

	"github.com/tencentcloud/CubeSandbox/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleImageAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: -1,
			RetMsg:  http.StatusText(http.StatusNotFound),
		},
	}
	defer func() {
		rt.RetCode = int64(rsp.Ret.RetCode)
	}()

	if r.Method == http.MethodPost {
		req := &types.CreateImageReq{}
		if err := utils.DecodeHttpBody(r.Body, req); err != nil {
			rsp.Ret.RetMsg = err.Error()
			return rsp
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
		rsp = sandbox.CreateImage(CubeLog.WithRequestTrace(ctx, rt), req)
	} else if r.Method == http.MethodDelete {
		req := &types.DeleteImageReq{}
		if err := utils.DecodeHttpBody(r.Body, req); err != nil {
			rsp.Ret.RetMsg = err.Error()
			return rsp
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
		rsp = sandbox.DeleteImage(CubeLog.WithRequestTrace(ctx, rt), req)
	}
	return rsp
}

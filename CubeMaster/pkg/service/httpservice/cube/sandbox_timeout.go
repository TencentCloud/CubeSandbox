// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
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

func handleSandboxTimeoutAction(_ http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &types.SandboxTimeoutRequest{}
	ctx, errRsp := decodeTTLRequest(r, rt, req, &req.RequestID, &req.SandboxID, &req.InstanceType)
	if errRsp != nil {
		return errRsp
	}
	out := sandbox.SetTimeout(ctx, req)
	rt.RetCode = int64(out.Ret.RetCode)
	return out
}

func handleSandboxRefreshAction(_ http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &types.SandboxRefreshRequest{}
	ctx, errRsp := decodeTTLRequest(r, rt, req, &req.RequestID, &req.SandboxID, &req.InstanceType)
	if errRsp != nil {
		return errRsp
	}
	out := sandbox.Refresh(ctx, req)
	rt.RetCode = int64(out.Ret.RetCode)
	return out
}

func decodeTTLRequest(
	r *http.Request, rt *CubeLog.RequestTrace, body any,
	requestID, sandboxID, instanceType *string,
) (context.Context, interface{}) {
	rt.RetCode = -1
	if err := utils.DecodeHttpBody(r.Body, body); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return nil, &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "decode body fail: " + err.Error(),
		}
	}
	if *requestID == "" {
		*requestID = uuid.New().String()
	}
	if *instanceType == "" {
		*instanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.RequestID = *requestID
	rt.InstanceType = *instanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    *requestID,
		"InstanceId":   *sandboxID,
		"InstanceType": *instanceType,
	}))
	return ctx, nil
}

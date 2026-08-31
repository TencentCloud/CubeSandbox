// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package trace

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func Report(ctx context.Context, callee, endpoint, action, calleeAction string, cost time.Duration,
	code errorcode.ErrorCode, errorCode CubeLog.ErrorCode) {

	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		return
	}

	rt = rt.DeepCopy()

	rt.Cost = cost
	if callee != "" {

		rt.Callee = callee
	}
	if endpoint != "" {

		rt.CalleeEndpoint = endpoint
	}
	if action != "" {

		rt.Action = action
	}
	if calleeAction != "" {

		rt.CalleeAction = calleeAction
	}

	rt.ErrorCode = errorCode
	rt.RetCode = int64(code)

	containerID, _ := ctx.Value(CubeLog.KeyContainerId).(string)
	ft, _ := ctx.Value(CubeLog.KeyFunctionType).(string)
	ns, _ := ctx.Value(CubeLog.KeyNamespace).(string)
	if containerID != "" {
		rt.ContainerID = containerID
	}
	if ft != "" {
		rt.FunctionType = ft
	}
	if ns != "" {
		rt.Namespace = ns
	}

	CubeLog.Trace(rt)
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// NormalizeSandboxIDParam resolves a short or full sandbox ID for HTTP handlers.
func NormalizeSandboxIDParam(ctx context.Context, sandboxID string) (string, *types.Ret) {
	return resolveSandboxIDOrError(ctx, sandboxID)
}

func resolveSandboxIDOrError(ctx context.Context, sandboxID string) (string, *types.Ret) {
	resolved, err := ResolveSandboxID(ctx, sandboxID)
	if err == nil {
		return resolved, nil
	}
	ret := &types.Ret{}
	switch {
	case errors.Is(err, sandboxid.ErrAmbiguous):
		ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		ret.RetMsg = err.Error()
	case errors.Is(err, sandboxid.ErrNotFound):
		ret.RetCode = int(errorcode.ErrorCode_NotFound)
		ret.RetMsg = err.Error()
	default:
		ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		ret.RetMsg = err.Error()
	}
	return "", ret
}

func normalizeSandboxIDInReq(ctx context.Context, sandboxID *string) *types.Ret {
	if sandboxID == nil || *sandboxID == "" {
		return nil
	}
	resolved, ret := resolveSandboxIDOrError(ctx, *sandboxID)
	if ret != nil {
		return ret
	}
	*sandboxID = resolved
	return nil
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

var afterDestroySandboxSuccess func(context.Context, string) error

func SetAfterDestroySandboxSuccessHook(hook func(context.Context, string) error) {
	afterDestroySandboxSuccess = hook
}

func runAfterDestroySandboxSuccessHook(ctx context.Context, sandboxID string) error {
	if afterDestroySandboxSuccess == nil {
		return nil
	}
	return afterDestroySandboxSuccess(ctx, sandboxID)
}

// CreateSandboxSuccessHook is invoked after a sandbox is successfully created
// on a cubelet node. Implementations should be cheap (or non-blocking) and
// MUST NOT cause the create path to fail when they error: the caller logs the
// error and continues.
type CreateSandboxSuccessHook func(ctx context.Context, sandboxID, hostID, hostIP string, req *types.CreateCubeSandboxReq) error

var afterCreateSandboxSuccess CreateSandboxSuccessHook

// SetAfterCreateSandboxSuccessHook registers a single hook to receive
// successful create events. Re-registering overwrites the previous hook.
func SetAfterCreateSandboxSuccessHook(hook CreateSandboxSuccessHook) {
	afterCreateSandboxSuccess = hook
}

func runAfterCreateSandboxSuccessHook(ctx context.Context, sandboxID, hostID, hostIP string, req *types.CreateCubeSandboxReq) error {
	if afterCreateSandboxSuccess == nil {
		return nil
	}
	return afterCreateSandboxSuccess(ctx, sandboxID, hostID, hostIP, req)
}

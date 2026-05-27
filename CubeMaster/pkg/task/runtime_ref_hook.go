// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import "context"

var afterDestroyTaskSuccess func(context.Context, string) error

func SetAfterDestroyTaskSuccessHook(hook func(context.Context, string) error) {
	afterDestroyTaskSuccess = hook
}

func runAfterDestroyTaskSuccessHook(ctx context.Context, sandboxID string) error {
	if afterDestroyTaskSuccess == nil {
		return nil
	}
	return afterDestroyTaskSuccess(ctx, sandboxID)
}

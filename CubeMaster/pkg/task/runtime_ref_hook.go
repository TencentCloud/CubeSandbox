// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

var (
	destroyTaskHooksMu sync.RWMutex
	destroyTaskHooks   []func(context.Context, string) error
)

// RegisterAfterDestroyTaskSuccessHook appends a hook fired after the async
// destroy task confirms the sandbox is gone. Hooks run sequentially.
func RegisterAfterDestroyTaskSuccessHook(hook func(context.Context, string) error) {
	if hook == nil {
		return
	}
	destroyTaskHooksMu.Lock()
	destroyTaskHooks = append(destroyTaskHooks, hook)
	destroyTaskHooksMu.Unlock()
}

// SetAfterDestroyTaskSuccessHook retained for backward compat: appends.
func SetAfterDestroyTaskSuccessHook(hook func(context.Context, string) error) {
	RegisterAfterDestroyTaskSuccessHook(hook)
}

// ResetAfterDestroyTaskSuccessHooks clears every registered hook (test-only).
func ResetAfterDestroyTaskSuccessHooks() {
	destroyTaskHooksMu.Lock()
	destroyTaskHooks = nil
	destroyTaskHooksMu.Unlock()
}

func runAfterDestroyTaskSuccessHook(ctx context.Context, sandboxID string) error {
	destroyTaskHooksMu.RLock()
	hooks := append([]func(context.Context, string) error(nil), destroyTaskHooks...)
	destroyTaskHooksMu.RUnlock()

	var firstErr error
	for _, h := range hooks {
		if err := h(ctx, sandboxID); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				log.G(ctx).Warnf("afterDestroyTaskSuccess hook chain error: %v", err)
			}
		}
	}
	return firstErr
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// destroyHooks holds every after-destroy callback registered at startup. We
// fan out to all of them on success so multiple subsystems (templatecenter,
// lifecycle metadata, ...) can react without stepping on each other.
var (
	destroyHooksMu sync.RWMutex
	destroyHooks   []func(context.Context, string) error
)

// RegisterAfterDestroySandboxSuccessHook appends a hook to the destroy chain.
// Hooks run sequentially in registration order; an individual hook's error is
// returned to the caller of runAfterDestroySandboxSuccessHook (joined when
// multiple hooks fail) but does NOT short-circuit later hooks.
func RegisterAfterDestroySandboxSuccessHook(hook func(context.Context, string) error) {
	if hook == nil {
		return
	}
	destroyHooksMu.Lock()
	destroyHooks = append(destroyHooks, hook)
	destroyHooksMu.Unlock()
}

// SetAfterDestroySandboxSuccessHook is retained for backward compatibility
// with single-registration callers (templatecenter). It now appends to the
// chain rather than replacing it; callers that genuinely need replacement
// semantics should use ResetAfterDestroySandboxSuccessHooks first.
func SetAfterDestroySandboxSuccessHook(hook func(context.Context, string) error) {
	RegisterAfterDestroySandboxSuccessHook(hook)
}

// ResetAfterDestroySandboxSuccessHooks clears every registered destroy hook.
// Test-only helper; production code never calls it.
func ResetAfterDestroySandboxSuccessHooks() {
	destroyHooksMu.Lock()
	destroyHooks = nil
	destroyHooksMu.Unlock()
}

func runAfterDestroySandboxSuccessHook(ctx context.Context, sandboxID string) error {
	destroyHooksMu.RLock()
	hooks := append([]func(context.Context, string) error(nil), destroyHooks...)
	destroyHooksMu.RUnlock()

	var firstErr error
	for _, h := range hooks {
		if err := h(ctx, sandboxID); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				log.G(ctx).Warnf("afterDestroySandboxSuccess hook chain error: %v", err)
			}
		}
	}
	return firstErr
}

// CreateSandboxSuccessHook is invoked after a sandbox is successfully created
// on a cubelet node. Implementations should be cheap (or non-blocking) and
// MUST NOT cause the create path to fail when they error: the caller logs the
// error and continues.
type CreateSandboxSuccessHook func(ctx context.Context, sandboxID, hostID, hostIP string, req *types.CreateCubeSandboxReq) error

var (
	createHooksMu sync.RWMutex
	createHooks   []CreateSandboxSuccessHook
)

// RegisterAfterCreateSandboxSuccessHook appends a hook to the create chain.
func RegisterAfterCreateSandboxSuccessHook(hook CreateSandboxSuccessHook) {
	if hook == nil {
		return
	}
	createHooksMu.Lock()
	createHooks = append(createHooks, hook)
	createHooksMu.Unlock()
}

// SetAfterCreateSandboxSuccessHook keeps the historical single-registration
// signature working: it appends rather than replaces.
func SetAfterCreateSandboxSuccessHook(hook CreateSandboxSuccessHook) {
	RegisterAfterCreateSandboxSuccessHook(hook)
}

// ResetAfterCreateSandboxSuccessHooks clears every registered create hook.
// Test-only helper.
func ResetAfterCreateSandboxSuccessHooks() {
	createHooksMu.Lock()
	createHooks = nil
	createHooksMu.Unlock()
}

func runAfterCreateSandboxSuccessHook(ctx context.Context, sandboxID, hostID, hostIP string, req *types.CreateCubeSandboxReq) error {
	createHooksMu.RLock()
	hooks := append([]CreateSandboxSuccessHook(nil), createHooks...)
	createHooksMu.RUnlock()

	var firstErr error
	for _, h := range hooks {
		if err := h(ctx, sandboxID, hostID, hostIP, req); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				log.G(ctx).Warnf("afterCreateSandboxSuccess hook chain error: %v", err)
			}
		}
	}
	return firstErr
}

// UpdateSandboxSuccessHook is invoked after a sandbox pause / resume RPC
// (POST /cube/sandbox/update) succeeds. Subscribers use it to fan out state
// transitions to downstream consumers such as the lifecycle metadata
// channel. The hook MUST NOT fail the update path — implementations should
// log and swallow errors.
//
// Action carries "pause" or "resume" (validated upstream). RequestID is
// forwarded from the incoming request for log correlation.
type UpdateSandboxSuccessHook func(ctx context.Context, sandboxID, instanceType, action, requestID string)

var (
	updateHooksMu sync.RWMutex
	updateHooks   []UpdateSandboxSuccessHook
)

// RegisterAfterUpdateSandboxSuccessHook appends a hook to the update chain.
// Hooks run sequentially in registration order.
func RegisterAfterUpdateSandboxSuccessHook(hook UpdateSandboxSuccessHook) {
	if hook == nil {
		return
	}
	updateHooksMu.Lock()
	updateHooks = append(updateHooks, hook)
	updateHooksMu.Unlock()
}

// ResetAfterUpdateSandboxSuccessHooks clears every registered update hook.
// Test-only helper.
func ResetAfterUpdateSandboxSuccessHooks() {
	updateHooksMu.Lock()
	updateHooks = nil
	updateHooksMu.Unlock()
}

func runAfterUpdateSandboxSuccessHook(ctx context.Context, sandboxID, instanceType, action, requestID string) {
	updateHooksMu.RLock()
	hooks := append([]UpdateSandboxSuccessHook(nil), updateHooks...)
	updateHooksMu.RUnlock()

	for _, h := range hooks {
		// Hooks are best-effort; recover in case a subscriber panics so
		// we can't take down the update path.
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.G(ctx).Warnf("afterUpdateSandboxSuccess hook panicked: %v", r)
				}
			}()
			h(ctx, sandboxID, instanceType, action, requestID)
		}()
	}
}

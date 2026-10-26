// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestDestroyHookChain_FIFOAndContinueOnError(t *testing.T) {
	ResetAfterDestroySandboxSuccessHooks()
	defer ResetAfterDestroySandboxSuccessHooks()

	var order []string
	wantErr := errors.New("h2 boom")

	RegisterAfterDestroySandboxSuccessHook(func(_ context.Context, id string) error {
		order = append(order, "h1:"+id)
		return nil
	})
	RegisterAfterDestroySandboxSuccessHook(func(_ context.Context, id string) error {
		order = append(order, "h2:"+id)
		return wantErr
	})
	RegisterAfterDestroySandboxSuccessHook(func(_ context.Context, id string) error {
		order = append(order, "h3:"+id)
		return nil
	})

	err := runAfterDestroySandboxSuccessHook(context.Background(), "sbx-x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("first error must propagate, got %v", err)
	}
	if got := order; len(got) != 3 || got[0] != "h1:sbx-x" || got[1] != "h2:sbx-x" || got[2] != "h3:sbx-x" {
		t.Fatalf("hooks ran out of order or stopped early: %v", got)
	}
}

func TestCreateHookChain_FIFO(t *testing.T) {
	ResetAfterCreateSandboxSuccessHooks()
	defer ResetAfterCreateSandboxSuccessHooks()

	var order []string
	RegisterAfterCreateSandboxSuccessHook(func(_ context.Context, id, _, _ string, _ *types.CreateCubeSandboxReq) error {
		order = append(order, "a:"+id)
		return nil
	})
	RegisterAfterCreateSandboxSuccessHook(func(_ context.Context, id, _, _ string, _ *types.CreateCubeSandboxReq) error {
		order = append(order, "b:"+id)
		return nil
	})

	if err := runAfterCreateSandboxSuccessHook(context.Background(), "sbx-y", "h-1", "1.2.3.4", &types.CreateCubeSandboxReq{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "a:sbx-y" || order[1] != "b:sbx-y" {
		t.Fatalf("hooks order wrong: %v", order)
	}
}

func TestNilHookIgnored(t *testing.T) {
	ResetAfterDestroySandboxSuccessHooks()
	defer ResetAfterDestroySandboxSuccessHooks()

	RegisterAfterDestroySandboxSuccessHook(nil)
	if err := runAfterDestroySandboxSuccessHook(context.Background(), "x"); err != nil {
		t.Fatalf("nil hook must be skipped silently, got %v", err)
	}
}

func TestUpdateHookChain_FIFO(t *testing.T) {
	ResetAfterUpdateSandboxSuccessHooks()
	defer ResetAfterUpdateSandboxSuccessHooks()

	type call struct {
		sandboxID    string
		instanceType string
		action       string
		requestID    string
	}
	var calls []call
	RegisterAfterUpdateSandboxSuccessHook(func(_ context.Context, sid, it, action, rid string) {
		calls = append(calls, call{sid, it, action, rid})
	})
	RegisterAfterUpdateSandboxSuccessHook(func(_ context.Context, sid, _, _, _ string) {
		calls = append(calls, call{sandboxID: "second:" + sid})
	})

	runAfterUpdateSandboxSuccessHook(context.Background(), "sbx-z", "cubebox", "pause", "req-1")
	if len(calls) != 2 {
		t.Fatalf("want 2 hook calls, got %d", len(calls))
	}
	if calls[0].sandboxID != "sbx-z" || calls[0].instanceType != "cubebox" ||
		calls[0].action != "pause" || calls[0].requestID != "req-1" {
		t.Fatalf("first hook received wrong args: %+v", calls[0])
	}
	if calls[1].sandboxID != "second:sbx-z" {
		t.Fatalf("second hook did not run: %+v", calls)
	}
}

func TestUpdateHook_PanicRecovered(t *testing.T) {
	ResetAfterUpdateSandboxSuccessHooks()
	defer ResetAfterUpdateSandboxSuccessHooks()

	RegisterAfterUpdateSandboxSuccessHook(func(_ context.Context, _, _, _, _ string) {
		panic("boom")
	})
	var reached bool
	RegisterAfterUpdateSandboxSuccessHook(func(_ context.Context, _, _, _, _ string) {
		reached = true
	})

	// Must not panic; must still run subsequent hooks.
	runAfterUpdateSandboxSuccessHook(context.Background(), "sbx-z", "cubebox", "resume", "req-2")
	if !reached {
		t.Fatalf("panicking hook must not prevent later hooks from running")
	}
}

func TestUpdateHook_NilIgnored(t *testing.T) {
	ResetAfterUpdateSandboxSuccessHooks()
	defer ResetAfterUpdateSandboxSuccessHooks()

	RegisterAfterUpdateSandboxSuccessHook(nil)
	runAfterUpdateSandboxSuccessHook(context.Background(), "x", "cubebox", "pause", "")
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import (
	"context"
	"errors"
	"testing"
)

func TestDestroyTaskHookChain_ForwardsReason(t *testing.T) {
	ResetAfterDestroyTaskSuccessHooks()
	defer ResetAfterDestroyTaskSuccessHooks()

	var gotID, gotReason string
	RegisterAfterDestroyTaskSuccessHook(func(_ context.Context, id, reason string) error {
		gotID, gotReason = id, reason
		return nil
	})

	if err := runAfterDestroyTaskSuccessHook(context.Background(), "sbx-a", "orphaned"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID != "sbx-a" || gotReason != "orphaned" {
		t.Fatalf("hook got id=%q reason=%q, want sbx-a/orphaned", gotID, gotReason)
	}
}

func TestDestroyTaskHookChain_ContinuesOnError(t *testing.T) {
	ResetAfterDestroyTaskSuccessHooks()
	defer ResetAfterDestroyTaskSuccessHooks()

	wantErr := errors.New("boom")
	var second bool
	RegisterAfterDestroyTaskSuccessHook(func(_ context.Context, _, _ string) error {
		return wantErr
	})
	RegisterAfterDestroyTaskSuccessHook(func(_ context.Context, _, _ string) error {
		second = true
		return nil
	})

	err := runAfterDestroyTaskSuccessHook(context.Background(), "sbx-b", "timeout")
	if !errors.Is(err, wantErr) {
		t.Fatalf("first error must propagate, got %v", err)
	}
	if !second {
		t.Fatalf("later hooks must still run after an error")
	}
}

func TestDestroyTaskHook_NilIgnored(t *testing.T) {
	ResetAfterDestroyTaskSuccessHooks()
	defer ResetAfterDestroyTaskSuccessHooks()

	RegisterAfterDestroyTaskSuccessHook(nil)
	if err := runAfterDestroyTaskSuccessHook(context.Background(), "x", "request"); err != nil {
		t.Fatalf("nil hook must be skipped silently, got %v", err)
	}
}

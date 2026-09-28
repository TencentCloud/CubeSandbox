// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package tombstone

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestRunPassSafe_RecoversPanic verifies a panic inside a purge pass is
// recovered by runPassSafe, so it can neither crash the host process nor
// terminate the janitor — run()'s loop continues to the next tick after a
// recovered panic. (A real, unrecovered panic here would crash the test binary.)
func TestRunPassSafe_RecoversPanic(t *testing.T) {
	orig := runPassFn
	t.Cleanup(func() { runPassFn = orig })
	runPassFn = func(_ context.Context, _ *gorm.DB, _ Config, _ *slog.Logger) {
		panic("simulated purge panic")
	}
	// Must return normally (panic recovered). If recovery were absent this
	// panic propagates and crashes the test process.
	runPassSafe(context.Background(), &gorm.DB{}, Config{}, slog.Default())
}

// TestStart_TablesFnOnlyNotNoOp guards against a regression where Start's guard
// returned early when only TablesFn (no static Tables) was provided — silently
// never starting the purger. CubeMaster wires exactly this way (TablesFn only),
// so a no-op here would mean the purger never runs in production.
func TestStart_TablesFnOnlyNotNoOp(t *testing.T) {
	orig := runPassFn
	t.Cleanup(func() { runPassFn = orig })
	invoked := make(chan struct{}, 1)
	runPassFn = func(_ context.Context, _ *gorm.DB, _ Config, _ *slog.Logger) {
		invoked <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Start(ctx, &gorm.DB{}, Config{
		Enabled:  true,
		LockName: "test_tablesfn_only",
		TablesFn: func() []string { return []string{"t_cube_sandbox_spec"} },
	}.sanitized())
	select {
	case <-invoked:
		// runPass was invoked → Start did not no-op.
	case <-time.After(2 * time.Second):
		t.Fatal("Start was a no-op: runPass never invoked when only TablesFn was set")
	}
}

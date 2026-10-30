// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package hook records sandbox lifecycle events from the sandbox success
// hooks. Every create, pause, resume and destroy passes through CubeMaster,
// including CLM auto-pause, auto-resume and timeout kills, so the history
// covers them as well as API calls.
package hook

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
)

// Init attaches the event store and registers the hooks. A failure only
// disables the history; it never stops CubeMaster. db.Init panics when the
// shared handle was never opened, so that is recovered here instead of
// relying on the call order in main.
func Init(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.G(ctx).Warnf("sandboxevent: history disabled: %v", r)
		}
	}()
	if err := sandboxevent.Init(db.Init(config.GetDbConfig())); err != nil {
		log.G(ctx).Warnf("sandboxevent: history disabled: %v", err)
		return
	}
	Register()
}

// Register adds the recording hooks to the sandbox hook chains. Both the
// synchronous and the asynchronous destroy path end with their own hook.
func Register() {
	sandbox.RegisterAfterCreateSandboxSuccessHook(onAfterCreate)
	sandbox.RegisterAfterDestroySandboxSuccessHook(onAfterDestroy)
	task.RegisterAfterDestroyTaskSuccessHook(onAfterDestroy)
	sandbox.RegisterAfterUpdateSandboxSuccessHook(onAfterUpdate)
}

func onAfterCreate(ctx context.Context, sandboxID, _, _ string, req *sandboxtypes.CreateCubeSandboxReq) error {
	templateID := ""
	if req != nil {
		templateID = req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	}
	sandboxevent.Record(ctx, sandboxID, sandboxevent.EventCreated, templateID, "")
	return nil
}

func onAfterDestroy(ctx context.Context, sandboxID string) error {
	sandboxevent.Record(ctx, sandboxID, sandboxevent.EventKilled, "", sandboxevent.KillReason(ctx))
	return nil
}

func onAfterUpdate(ctx context.Context, sandboxID, _, action, _ string) {
	switch action {
	case "pause":
		sandboxevent.Record(ctx, sandboxID, sandboxevent.EventPaused, "", "")
	case "resume":
		sandboxevent.Record(ctx, sandboxID, sandboxevent.EventResumed, "", "")
	}
}

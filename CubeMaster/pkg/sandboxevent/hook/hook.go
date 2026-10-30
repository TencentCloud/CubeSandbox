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
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
)

// retryInterval is how often a failed store setup is retried, e.g. while the
// table is still being created out-of-band (CUBE_AUTO_MIGRATION=false).
var retryInterval = 30 * time.Second

// Init registers the hooks and attaches the event store. Recording is a
// no-op until the store is ready, so a failed setup never stops CubeMaster;
// it is retried in the background until it succeeds.
func Init(ctx context.Context) {
	Register()
	if attach(ctx, true) {
		return
	}
	go func() {
		ticker := time.NewTicker(retryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if attach(ctx, false) {
					log.G(ctx).Infof("sandboxevent: history enabled")
					return
				}
			}
		}
	}()
}

// attach tries to set up the store once. db.Init panics when the shared
// handle was never opened, so that is recovered instead of relying on the
// call order in main. Only the first failure is a warning; a table managed
// elsewhere may stay missing for a long time.
func attach(ctx context.Context, first bool) (ok bool) {
	report := func(err any) {
		if first {
			log.G(ctx).Warnf("sandboxevent: history disabled, retrying every %s: %v", retryInterval, err)
		} else {
			log.G(ctx).Debugf("sandboxevent: history still disabled: %v", err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			report(r)
			ok = false
		}
	}()
	if err := sandboxevent.Init(db.Init(config.GetDbConfig())); err != nil {
		report(err)
		return false
	}
	return true
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

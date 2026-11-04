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
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"gorm.io/gorm"
)

// retryInterval is how often a failed store setup is retried, e.g. while the
// table is still being created out-of-band (CUBE_AUTO_MIGRATION=false).
var retryInterval = 30 * time.Second

// Init registers the hooks and attaches the event store to client. Recording
// is a no-op until the store is ready. When the first attempt fails (e.g. the
// table is still being created out-of-band with CUBE_AUTO_MIGRATION=false),
// the error is returned for the caller to report and the setup is retried in
// the background until it succeeds or ctx ends.
func Init(ctx context.Context, client *gorm.DB) error {
	Register()
	err := sandboxevent.Init(client)
	if err == nil {
		return nil
	}
	go func() {
		ticker := time.NewTicker(retryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := sandboxevent.Init(client); err != nil {
					log.G(ctx).Debugf("sandboxevent: history still disabled: %v", err)
					continue
				}
				log.G(ctx).Infof("sandboxevent: history enabled")
				return
			}
		}
	}()
	return err
}

var registerOnce sync.Once

// Register adds the recording hooks to the sandbox hook chains. Both the
// synchronous and the asynchronous destroy path end with their own hook.
// Only the first call registers, so events are never recorded twice.
func Register() {
	registerOnce.Do(func() {
		sandbox.RegisterAfterCreateSandboxSuccessHook(onAfterCreate)
		sandbox.RegisterAfterDestroySandboxSuccessHook(onAfterDestroy)
		task.RegisterAfterDestroyTaskSuccessHook(onAfterDestroy)
		sandbox.RegisterAfterUpdateSandboxSuccessHook(onAfterUpdate)
	})
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package templatecenter

import (
	"context"

	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

var lifecyclePublisher webhook.PublisherHolder

func SetWebhookPublisher(p webhook.Publisher) { lifecyclePublisher.Set(p) }

type checkpointNotificationKey struct{}

type checkpointNotification struct {
	sandboxID, templateID string
	fields                webhook.LifecycleFields
}

func withCheckpointNotification(ctx context.Context, req *sandboxtypes.CreateCubeSandboxReq, sandboxID string) context.Context {
	if !lifecyclePublisher.Enabled() {
		return ctx
	}
	template, fields := webhook.PrepareLifecycle(req)
	return context.WithValue(ctx, checkpointNotificationKey{}, checkpointNotification{sandboxID, template, fields})
}

func publishCheckpoint(ctx context.Context) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	if notification, ok := ctx.Value(checkpointNotificationKey{}).(checkpointNotification); ok {
		lifecyclePublisher.Publish(webhook.TypeCheckpointed, notification.sandboxID, notification.templateID, notification.fields)
	}
}

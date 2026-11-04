// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

var lifecyclePublisher webhook.PublisherHolder

func SetWebhookPublisher(p webhook.Publisher) { lifecyclePublisher.Set(p) }

func WebhooksEnabled() bool { return lifecyclePublisher.Enabled() }

// PublishCreatedSandbox observes the public create result and trusted resolution.
// Metadata was captured locally before template merging; it is never stored.
// The caller has observed business success; later request cancellation does not
// undo that result. Wait already arbitrates cancellation against success.
func PublishCreatedSandbox(ctx context.Context, req *types.CreateCubeSandboxReq, sandboxID, templateID string, snapshot bool, metadata webhook.LifecycleFields) {
	if !lifecyclePublisher.Enabled() || sandboxID == "" {
		return
	}
	_, fields := webhook.PrepareLifecycle(req)
	fields.Metadata, fields.TooLarge = metadata.Metadata, fields.TooLarge || metadata.TooLarge
	kind := webhook.TypeCreated
	if snapshot {
		kind = webhook.TypeResumed
		fields.ResumeMode = "restore"
	}
	lifecyclePublisher.PublishContext(ctx, kind, sandboxID, templateID, fields)
}

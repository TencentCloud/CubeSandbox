// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package templatecenter

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

var lifecyclePublisher webhook.PublisherHolder

func SetWebhookPublisher(p webhook.Publisher) { lifecyclePublisher.Set(p) }

type snapshotWebhook struct {
	template string
	fields   webhook.LifecycleFields
}

// PublishTemplateEvent publishes a template lifecycle webhook event.
// This is the central entry point for all template lifecycle events
// (build started/completed/failed, deleted, migrated).
func PublishTemplateEvent(ctx context.Context, eventType, templateID, buildID string, fields webhook.LifecycleFields) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	lifecyclePublisher.PublishContext(ctx, eventType, "", templateID, fields)
}

// PublishTemplateBuildStarted publishes the template.lifecycle.build.started event.
func PublishTemplateBuildStarted(ctx context.Context, templateID, buildID, sourceImageRef, instanceType, buildMode string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		SourceImageRef: sourceImageRef,
		InstanceType:   instanceType,
		BuildMode:      buildMode,
	})
	// Use buildID as the BuildID in CaptureInput for correlation
	lifecyclePublisher.PublishContext(withBuildID(ctx, buildID), webhook.TypeTemplateBuildStarted, "", templateID, fields)
}

// PublishTemplateBuildCompleted publishes the template.lifecycle.build.completed event.
func PublishTemplateBuildCompleted(ctx context.Context, templateID, buildID, sourceImageRef, instanceType, buildMode, artifactID string, readyNodeCount int32) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		SourceImageRef: sourceImageRef,
		InstanceType:   instanceType,
		BuildMode:      buildMode,
		ArtifactID:     artifactID,
		ReadyNodeCount: readyNodeCount,
	})
	lifecyclePublisher.PublishContext(withBuildID(ctx, buildID), webhook.TypeTemplateBuildCompleted, "", templateID, fields)
}

// PublishTemplateBuildFailed publishes the template.lifecycle.build.failed event.
func PublishTemplateBuildFailed(ctx context.Context, templateID, buildID, sourceImageRef, instanceType, buildMode, errorMsg string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		SourceImageRef: sourceImageRef,
		InstanceType:   instanceType,
		BuildMode:      buildMode,
		ErrorMsg:       errorMsg,
	})
	lifecyclePublisher.PublishContext(withBuildID(ctx, buildID), webhook.TypeTemplateBuildFailed, "", templateID, fields)
}

// PublishTemplateDeleted publishes the template.lifecycle.deleted event.
func PublishTemplateDeleted(ctx context.Context, templateID, instanceType string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		InstanceType: instanceType,
	})
	lifecyclePublisher.PublishContext(ctx, webhook.TypeTemplateDeleted, "", templateID, fields)
}

// PublishSnapshotDeleted publishes the template.lifecycle.snapshot.deleted event.
func PublishSnapshotDeleted(ctx context.Context, snapshotID, instanceType, errorMsg string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		InstanceType: instanceType,
		ErrorMsg:     errorMsg,
	})
	lifecyclePublisher.PublishContext(ctx, webhook.TypeSnapshotDeleted, "", snapshotID, fields)
}

// PublishSnapshotRollback publishes the template.lifecycle.snapshot.rollback.completed event.
func PublishSnapshotRollback(ctx context.Context, snapshotID, sandboxID string, newGen uint32) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		NewGen: newGen,
	})
	lifecyclePublisher.PublishContext(ctx, webhook.TypeSnapshotRollback, sandboxID, snapshotID, fields)
}

// withBuildID embeds a buildID into the context for use as Origin context metadata.
// This allows the build ID to be carried through to the webhook event capture.
func withBuildID(ctx context.Context, buildID string) context.Context {
	if buildID == "" {
		return ctx
	}
	return context.WithValue(ctx, buildIDContextKey{}, buildID)
}

type buildIDContextKey struct{}

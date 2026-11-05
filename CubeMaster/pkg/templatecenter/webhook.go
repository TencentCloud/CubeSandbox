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
	lifecyclePublisher.PublishContext(webhook.WithBuildID(ctx, buildID), webhook.TypeTemplateBuildStarted, "", templateID, fields)
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
	lifecyclePublisher.PublishContext(webhook.WithBuildID(ctx, buildID), webhook.TypeTemplateBuildCompleted, "", templateID, fields)
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
	lifecyclePublisher.PublishContext(webhook.WithBuildID(ctx, buildID), webhook.TypeTemplateBuildFailed, "", templateID, fields)
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

// PublishTemplateMigrated publishes the template.lifecycle.migrated event once a
// redo has redistributed a template to its new node set and the job reached a
// terminal READY status.
func PublishTemplateMigrated(ctx context.Context, templateID, buildID, instanceType string, readyNodeCount int32) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		InstanceType:   instanceType,
		ReadyNodeCount: readyNodeCount,
	})
	lifecyclePublisher.PublishContext(webhook.WithBuildID(ctx, buildID), webhook.TypeTemplateMigrated, "", templateID, fields)
}

// PublishSnapshotCreated publishes the template.lifecycle.snapshot.created event.
// It fires at the same boundary as sandbox.lifecycle.checkpointed: the snapshot
// exists and the template row is READY.
func PublishSnapshotCreated(ctx context.Context, snapshotID, sandboxID, templateID, backend string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		InstanceType: backend,
	})
	lifecyclePublisher.PublishSnapshotContext(ctx, webhook.TypeSnapshotCreated, sandboxID, templateID, snapshotID, fields)
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
	lifecyclePublisher.PublishSnapshotContext(ctx, webhook.TypeSnapshotDeleted, "", "", snapshotID, fields)
}

// PublishSnapshotRollback publishes the template.lifecycle.snapshot.rollback.completed event.
func PublishSnapshotRollback(ctx context.Context, snapshotID, sandboxID string, newGen uint32) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	fields := webhook.PrepareTemplateLifecycle(webhook.TemplateLifecycleOpts{
		NewGen: newGen,
	})
	lifecyclePublisher.PublishSnapshotContext(ctx, webhook.TypeSnapshotRollback, sandboxID, "", snapshotID, fields)
}

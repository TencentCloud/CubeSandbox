// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package templatecenter

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
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

// PublishTemplateBuildFailedForJob publishes template.lifecycle.build.failed for
// a job that has just been persisted as FAILED, rebuilding the build metadata
// from the job row. Failure paths do not all carry the original request (the
// builder callback only knows the job id, and the resume/reconcile paths keep
// the request in different shapes), so the row is the one place the source
// image, instance type and error message are always available.
func PublishTemplateBuildFailedForJob(ctx context.Context, jobID string) {
	if !lifecyclePublisher.Enabled() {
		return
	}
	job, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		log.G(ctx).Warnf("publish template build failed: load job fail: job_id=%s err=%v", jobID, err)
		return
	}
	PublishTemplateBuildFailed(ctx, job.TemplateID, jobID, job.SourceImageRef, job.InstanceType,
		templateBuildMode(job), job.ErrorMessage)
}

// templateBuildMode rebuilds the build mode carried by build.started so a
// failure event describes the same build as its start event.
func templateBuildMode(job *models.TemplateImageJob) string {
	mode := "remote"
	if job.Operation == JobOperationRedo {
		mode = "redo"
	}
	if job.InstanceType != "" {
		mode += ":" + job.InstanceType
	}
	return mode
}

// failTemplateImageJob persists the terminal FAILED state of a build job and
// publishes template.lifecycle.build.failed once the write lands, so a write
// that failed cannot announce a state that was never persisted.
func failTemplateImageJob(ctx context.Context, jobID string, values map[string]any) error {
	values["status"] = JobStatusFailed
	if err := updateTemplateImageJob(ctx, jobID, values); err != nil {
		return err
	}
	PublishTemplateBuildFailedForJob(ctx, jobID)
	return nil
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

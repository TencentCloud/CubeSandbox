// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func getActiveTemplateMigrateJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND operation = ? AND status IN ?", templateID, JobOperationMigrate, []string{JobStatusPending, JobStatusRunning}).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func getLatestTemplateMigrateJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND operation = ?", templateID, JobOperationMigrate).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

// SubmitTemplateMigrate submits (or reuses) a template migrate job and starts
// the async executor when a fresh job is created.
func SubmitTemplateMigrate(ctx context.Context, templateID, requestID string) (*types.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return nil, ErrTemplateIDRequired
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = uuid.NewString()
	}

	jobID := uuid.NewString()
	reusedExistingJob := false
	if err := withTemplateWriteLock(templateID, func() error {
		def, err := GetDefinition(ctx, templateID)
		if err != nil {
			return err
		}
		if def.Status == StatusDeleting {
			return ErrTemplateNotFound
		}
		if def.Status != StatusReady {
			return ErrTemplateNotReady
		}
		artifactID := strings.TrimSpace(def.RootfsArtifactID)
		if artifactID == "" {
			return fmt.Errorf("template %s has no rootfs artifact id", templateID)
		}

		if job, err := getActiveTemplateMigrateJobByTemplateID(ctx, templateID); err == nil {
			jobID = job.JobID
			reusedExistingJob = true
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		attemptNo := int32(1)
		retryOfJobID := ""
		if latestJob, err := getLatestTemplateMigrateJobByTemplateID(ctx, templateID); err == nil {
			attemptNo = nextAttemptNoFromLatest(latestJob.AttemptNo)
			retryOfJobID = latestJob.JobID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		record := &models.TemplateImageJob{
			JobID:        jobID,
			TemplateID:   templateID,
			RequestID:    requestID,
			AttemptNo:    attemptNo,
			RetryOfJobID: retryOfJobID,
			Operation:    JobOperationMigrate,
			ArtifactID:   artifactID,
			Status:       JobStatusPending,
			Phase:        JobPhaseMigratingArtifact,
			Progress:     0,
		}
		return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
	}); err != nil {
		return nil, err
	}

	if !reusedExistingJob {
		go runTemplateMigrateJob(detachTemplateImageJobContext(ctx, "template_migrate", map[string]any{
			"job_id":      jobID,
			"template_id": templateID,
		}), jobID, templateID)
	}
	return GetTemplateImageJobInfo(ctx, jobID)
}

// GetTemplateMigrateJobInfo returns one migrate job by job_id.
func GetTemplateMigrateJobInfo(ctx context.Context, jobID string) (*types.TemplateImageJobInfo, error) {
	info, err := GetTemplateImageJobInfo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if info == nil || info.Operation != JobOperationMigrate {
		return nil, fmt.Errorf("%w: migrate job_id=%s", ErrTemplateImageJobNotFound, jobID)
	}
	return info, nil
}

func runTemplateMigrateJob(ctx context.Context, jobID, templateID string) {
	logger := log.G(ctx).WithFields(map[string]any{"job_id": jobID, "template_id": templateID})
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseMigratingArtifact,
		"progress": 20,
	}); err != nil {
		logger.Errorf("mark migrate job running fail: %v", err)
		return
	}

	result, err := MigrateTemplateArtifactToTC(ctx, templateID)
	if err != nil {
		logger.Errorf("migrate template artifact fail: %v", err)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseMigratingArtifact,
			"progress":      100,
			"error_message": err.Error(),
		})
		return
	}

	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":     result.ArtifactID,
		"status":          JobStatusReady,
		"phase":           JobPhaseReady,
		"progress":        100,
		"artifact_status": ArtifactStatusReady,
		"error_message":   "",
	}); err != nil {
		logger.Errorf("mark migrate job ready fail: %v", err)
		return
	}
	logger.Infof("template artifact migrated: artifact_id=%s migrated=%t cleaned=%t", result.ArtifactID, result.Migrated, result.Cleaned)
}

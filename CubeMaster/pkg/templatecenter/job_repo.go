// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"time"
)

func getTemplateImageJobRecordByID(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).First(record).Error; err != nil {
		return nil, err
	}
	return record, nil
}

func getLatestTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	return getLatestTemplateImageJobByTemplateID(ctx, templateID)
}

func getActiveTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND status IN ?", templateID, []string{JobStatusPending, JobStatusRunning}).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func getTemplateImageJobByRequestID(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("request_id = ?", requestID).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func getActiveSnapshotJobBySandboxID(ctx context.Context, sandboxID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("sandbox_id = ? AND operation IN ? AND status IN ?", sandboxID,
			[]string{JobOperationSnapshotCreate, JobOperationSnapshotRollback},
			[]string{JobStatusPending, JobStatusRunning}).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func getActiveSnapshotJobByResourceID(ctx context.Context, resourceID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("resource_id = ? AND operation IN ? AND status IN ?", resourceID,
			[]string{JobOperationSnapshotCreate, JobOperationSnapshotRollback, JobOperationSnapshotDelete},
			[]string{JobStatusPending, JobStatusRunning}).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func listTemplateImageJobsByTemplateID(ctx context.Context, templateID string) ([]models.TemplateImageJob, error) {
	var records []models.TemplateImageJob
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).
		Order("attempt_no desc, id desc").Find(&records).Error
	return records, err
}

func getRootfsArtifactByID(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByIDUnscoped(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByFingerprint(ctx context.Context, fingerprint string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func getRootfsArtifactByFingerprintUnscoped(ctx context.Context, fingerprint string) (*models.RootfsArtifact, error) {
	record := &models.RootfsArtifact{}
	err := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).First(record).Error
	if err != nil {
		return nil, err
	}
	return record, err
}

func updateTemplateImageJob(ctx context.Context, jobID string, values map[string]any) error {
	values["updated_at"] = time.Now()
	tx := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).Updates(values)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func updateRootfsArtifact(ctx context.Context, artifactID string, values map[string]any) error {
	values["updated_at"] = time.Now()
	tx := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).Updates(values)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

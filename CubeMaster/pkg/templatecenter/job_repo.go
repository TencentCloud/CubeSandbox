// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

func getTemplateImageJobRecordByID(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	if err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).First(record).Error; err != nil {
		return nil, err
	}
	return record, nil
}

// ErrTerminalJobStatusFlip is returned when a status callback tries to move a
// job OUT of a terminal state. The callback endpoint is unauthenticated, so its
// payloads are untrusted: without this guard any caller that can reach the
// internal route could flip an already-READY or already-FAILED job back to
// RUNNING/BUILT, resurrecting completed work or re-triggering the pipeline.
var ErrTerminalJobStatusFlip = errors.New("refusing to move a terminal template job back to a non-terminal status")

// templateJobTerminal reports whether a job status is terminal.
func templateJobTerminal(status string) bool {
	return status == JobStatusReady || status == JobStatusFailed
}

// ValidateTemplateJobStatusTransition checks that applying `newStatus` to job
// `jobID` is a legal transition. It exists to stop an unauthenticated status
// callback from flipping a terminal job back to life. The check is best-effort:
// a missing job (or an unreadable store) does not block the update, since the
// callback must still work for the very first report of a job.
//
// Allowed: any transition whose current state is NOT terminal, plus idempotent
// re-reporting of the same terminal state (a retried READY/FAILED callback is
// harmless). Forbidden: terminal -> a DIFFERENT status.
func ValidateTemplateJobStatusTransition(ctx context.Context, jobID, newStatus string) error {
	if !isReady() {
		return nil
	}
	job, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return nil // unreadable store: do not block the callback on a lookup error
	}
	if templateJobTerminal(job.Status) && job.Status != newStatus {
		return fmt.Errorf("%w: job %s is %s, cannot move to %s", ErrTerminalJobStatusFlip, jobID, job.Status, newStatus)
	}
	return nil
}

func getLatestTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	return getLatestTemplateImageJobByTemplateIDTx(store.db.WithContext(ctx), templateID)
}

func getLatestTemplateImageJobByTemplateIDTx(tx *gorm.DB, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := tx.Table(constants.TemplateImageJobTableName).
		Where("template_id = ?", templateID).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func getLatestCreateRedoImageJobByTemplateIDTx(tx *gorm.DB, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := tx.Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND operation IN ?", templateID, createRedoJobOperations).
		Order("attempt_no desc, id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func listCreateRedoImageJobsByTemplateIDTx(tx *gorm.DB, templateID string) ([]models.TemplateImageJob, error) {
	var records []models.TemplateImageJob
	err := tx.Table(constants.TemplateImageJobTableName).
		Select("job_id, operation, request_json").
		Where("template_id = ? AND operation IN ?", templateID, createRedoJobOperations).
		Order("attempt_no desc, id desc").Find(&records).Error
	return records, err
}

func getTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	return getLatestTemplateImageJobByTemplateID(ctx, templateID)
}

func getActiveTemplateImageJobByTemplateID(ctx context.Context, templateID string) (*models.TemplateImageJob, error) {
	return getActiveTemplateImageJobByTemplateIDTx(store.db.WithContext(ctx), templateID)
}

func getActiveTemplateImageJobByTemplateIDTx(tx *gorm.DB, templateID string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := tx.Table(constants.TemplateImageJobTableName).
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
	return updateTemplateImageJobTx(store.db.WithContext(ctx), jobID, values)
}

func updateTemplateImageJobTx(tx *gorm.DB, jobID string, values map[string]any) error {
	values["updated_at"] = time.Now()
	result := tx.Table(constants.TemplateImageJobTableName).
		Where("job_id = ?", jobID).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateTemplateImageJob exports updateTemplateImageJob for the internal
// status-callback handler used by the remote build mode (CubeTemplateCenter
// reports job progress back to CubeMaster).
func UpdateTemplateImageJob(ctx context.Context, jobID string, values map[string]any) error {
	return updateTemplateImageJob(ctx, jobID, values)
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	migrateJobStaleAfter = 10 * time.Minute

	// migrateAttemptInsertMaxAttempts bounds the duplicate-attempt recovery
	// loop. attempt_no is unique across all operations for a template, so a
	// concurrent create/redo/merge on another replica can still win the next
	// number after we read it; retrying a small, deterministic number of times
	// turns that race into a successful insert instead of surfacing 1062.
	migrateAttemptInsertMaxAttempts = 4

	// migrateJobHeartbeatInterval must stay well below migrateJobStaleAfter.
	// The runner writes the job row only when it starts and when it finishes,
	// so without a heartbeat `updated_at` freezes for the whole transfer and
	// failStaleActiveTemplateMigrateJob (which is driven by updated_at) would
	// fail a job that is still uploading and let the next submit start a second,
	// concurrent migration of the same artifact.
	migrateJobHeartbeatInterval = 1 * time.Minute
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

func failStaleActiveTemplateMigrateJob(ctx context.Context, templateID string, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-staleAfter)
	return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id = ? AND operation = ? AND status IN ? AND updated_at < ?", templateID, JobOperationMigrate, []string{JobStatusPending, JobStatusRunning}, cutoff).
		Updates(map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseMigratingArtifact,
			"progress":      100,
			"error_message": fmt.Sprintf("stale migrate job marked failed after %s", staleAfter),
		}).Error
}

// failAllStaleTemplateMigrateJobs sweeps stale MIGRATE rows across ALL
// templates. MIGRATE jobs are executed by an in-process CubeMaster goroutine,
// so the "PENDING/RUNNING belongs to TC" boundary the image-job reconciler
// observes for build jobs does not apply here: a CubeMaster crash mid-upload
// strands the row, and without this sweep every later create/redo for that
// template keeps failing with ErrTemplateAttemptInProgress until someone
// happens to re-run `tpl merge` (the only other caller of the stale check).
func failAllStaleTemplateMigrateJobs(ctx context.Context, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-staleAfter)
	return store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("operation = ? AND status IN ? AND updated_at < ?", JobOperationMigrate, []string{JobStatusPending, JobStatusRunning}, cutoff).
		Updates(map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseMigratingArtifact,
			"progress":      100,
			"error_message": fmt.Sprintf("stale migrate job marked failed after %s (reconciler sweep)", staleAfter),
		}).Error
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

// beforeTemplateMigrateJobInsert is a test seam for reproducing the cross-
// replica race between "read latest attempt_no" and INSERT.
var beforeTemplateMigrateJobInsert = func(ctx context.Context, record *models.TemplateImageJob) {}

func latestTemplateAttemptNo(ctx context.Context, templateID string) (int32, error) {
	latestJob, err := getLatestTemplateImageJobByTemplateID(ctx, templateID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return latestJob.AttemptNo, nil
}

// insertTemplateMigrateJobWithAttemptRetry creates a migrate job using the
// next attempt number across ALL template job operations, then recovers from
// the one race withTemplateWriteLock cannot cover: a peer process inserting a
// job for the same template after this process read the latest attempt.
func insertTemplateMigrateJobWithAttemptRetry(ctx context.Context, templateID string, record *models.TemplateImageJob) error {
	latestAttemptNo, err := latestTemplateAttemptNo(ctx, templateID)
	if err != nil {
		return err
	}
	record.AttemptNo = nextAttemptNoFromLatest(latestAttemptNo)

	var createErr error
	for attempt := 0; attempt < migrateAttemptInsertMaxAttempts; attempt++ {
		beforeTemplateMigrateJobInsert(ctx, record)
		createErr = store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error
		if createErr == nil {
			return nil
		}
		if !isDuplicateKeyError(createErr) {
			return createErr
		}
		latestAttemptNo, err = latestTemplateAttemptNo(ctx, templateID)
		if err != nil {
			return errors.Join(createErr, fmt.Errorf("reload latest attempt for template %s: %w", templateID, err))
		}
		nextAttemptNo := nextAttemptNoFromLatest(latestAttemptNo)
		if nextAttemptNo <= record.AttemptNo {
			nextAttemptNo = record.AttemptNo + 1
		}
		record.AttemptNo = nextAttemptNo
	}
	return fmt.Errorf("create migrate job for template %s after resolving attempt conflicts: %w", templateID, createErr)
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

		if err := failStaleActiveTemplateMigrateJob(ctx, templateID, migrateJobStaleAfter); err != nil {
			return err
		}

		if job, err := getActiveTemplateMigrateJobByTemplateID(ctx, templateID); err == nil {
			jobID = job.JobID
			reusedExistingJob = true
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// retry_of_job_id stays migrate-scoped, while attempt_no is assigned by
		// insertTemplateMigrateJobWithAttemptRetry across all job operations.
		retryOfJobID := ""
		if latestMigrateJob, err := getLatestTemplateMigrateJobByTemplateID(ctx, templateID); err == nil {
			retryOfJobID = latestMigrateJob.JobID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		record := &models.TemplateImageJob{
			JobID:        jobID,
			TemplateID:   templateID,
			RequestID:    requestID,
			RetryOfJobID: retryOfJobID,
			Operation:    JobOperationMigrate,
			ArtifactID:   artifactID,
			Status:       JobStatusPending,
			Phase:        JobPhaseMigratingArtifact,
			Progress:     0,
		}
		if err := insertTemplateMigrateJobWithAttemptRetry(ctx, templateID, record); err != nil {
			return err
		}

		// Cross-replica race guard. withTemplateWriteLock only serializes
		// submissions within THIS CubeMaster process; the
		// getActiveTemplateMigrateJobByTemplateID check above reads the
		// shared DB, but two replicas can still both observe "no active job"
		// before either INSERT commits, and both create a row here. Re-check
		// right after the insert and yield to whichever row is earliest by
		// id: every racing replica runs this same query and every loser sees
		// a smaller-id winner, so at most one of them proceeds to spawn a
		// migrate goroutine.
		earliest := &models.TemplateImageJob{}
		qerr := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
			Where("template_id = ? AND operation = ? AND status IN ?", templateID, JobOperationMigrate, []string{JobStatusPending, JobStatusRunning}).
			Order("id asc").First(earliest).Error
		if qerr != nil && !errors.Is(qerr, gorm.ErrRecordNotFound) {
			return qerr
		}
		if earliest.JobID != "" && earliest.JobID != jobID {
			if uerr := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
				Where("job_id = ?", jobID).
				Updates(map[string]any{
					"status":        JobStatusFailed,
					"progress":      100,
					"error_message": fmt.Sprintf("superseded by concurrently-created migrate job %s on another replica", earliest.JobID),
				}).Error; uerr != nil {
				return uerr
			}
			jobID = earliest.JobID
			reusedExistingJob = true
		}
		return nil
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

// startMigrateJobHeartbeat keeps the job row's updated_at advancing while the
// artifact is being uploaded.
//
// MigrateTemplateArtifactToTC only touches the artifact row during the
// transfer, so without this the job row looks frozen from the moment the
// runner starts until it completes. failStaleActiveTemplateMigrateJob reads
// updated_at, so a transfer longer than migrateJobStaleAfter would be failed
// by the next submit even though it is still healthy -- and that submit would
// then spawn a second runner uploading the same artifact.
//
// The returned stop function waits for the goroutine to exit so the job row is
// never written after the runner has returned.
func startMigrateJobHeartbeat(ctx context.Context, jobID string) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		// A panic here must not take the process down: like every other job
		// runner in this package, the heartbeat owns nothing and only logs.
		defer func() {
			if r := recover(); r != nil {
				log.G(ctx).Errorf("migrate job %s heartbeat panic: %v\n%s", jobID, r, debug.Stack())
			}
		}()
		ticker := time.NewTicker(migrateJobHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				// The heartbeat goes through the guarded transition helper with
				// RUNNING as the allowed predecessor: once the runner (or the
				// stale sweep, or a cross-replica supersede) has written a
				// terminal status, this write is refused instead of stamping
				// MIGRATING_ARTIFACT back onto a finished job. A refused write
				// also means there is nothing left to heartbeat, so the
				// goroutine exits.
				if err := UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
					"status": JobStatusRunning,
					"phase":  JobPhaseMigratingArtifact,
				}, JobStatusRunning); err != nil {
					if errors.Is(err, ErrTerminalJobStatusFlip) {
						return
					}
					log.G(ctx).Warnf("migrate job %s heartbeat fail: %v", jobID, err)
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

func runTemplateMigrateJob(ctx context.Context, jobID, templateID string) {
	logger := log.G(ctx).WithFields(map[string]any{"job_id": jobID, "template_id": templateID})
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("template migrate job panic: %v\n%s", r, debug.Stack())
			_ = UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
				"status":        JobStatusFailed,
				"phase":         JobPhaseMigratingArtifact,
				"progress":      100,
				"error_message": fmt.Sprintf("template migrate job panic: %v", r),
			}, JobStatusFailed)
		}
	}()

	// Marking RUNNING is a guarded transition, not a blind write: a job that the
	// cross-replica supersede (SubmitTemplateMigrate) or the stale sweep already
	// finished must not be brought back to life, or this process would upload
	// the same artifact a second time in parallel.
	if err := UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseMigratingArtifact,
		"progress": 20,
	}, JobStatusRunning); err != nil {
		logger.Errorf("mark migrate job running fail: %v", err)
		if errors.Is(err, ErrTerminalJobStatusFlip) {
			return
		}
		_ = UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseMigratingArtifact,
			"progress":      100,
			"error_message": fmt.Sprintf("mark migrate job running failed: %v", err),
		}, JobStatusFailed)
		return
	}

	stopHeartbeat := startMigrateJobHeartbeat(ctx, jobID)
	defer stopHeartbeat()

	result, err := MigrateTemplateArtifactToTC(ctx, templateID)
	if err != nil {
		logger.Errorf("migrate template artifact fail: %v", err)
		_ = UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseMigratingArtifact,
			"progress":      100,
			"error_message": err.Error(),
		}, JobStatusFailed)
		return
	}

	// Same guard on the way out: a runner that outlived the stale window must
	// not flip its already-FAILED job back to READY and report the migration as
	// successful while a second attempt exists.
	if err := UpdateTemplateImageJobIfTransitionAllowed(ctx, jobID, map[string]any{
		"artifact_id":     result.ArtifactID,
		"status":          JobStatusReady,
		"phase":           JobPhaseReady,
		"progress":        100,
		"artifact_status": ArtifactStatusReady,
		"error_message":   "",
	}, JobStatusReady); err != nil {
		logger.Errorf("mark migrate job ready fail: %v", err)
		return
	}
	logger.Infof("template artifact migrated: artifact_id=%s migrated=%t cleaned=%t", result.ArtifactID, result.Migrated, result.Cleaned)
}

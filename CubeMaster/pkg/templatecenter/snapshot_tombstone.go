// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	snapshotOrphanRuntimeRefTTL = 24 * time.Hour
	snapshotTombstoneRequestID  = "tombstone-finalize:"
)

var (
	sandboxExistsOnMasterFn = sandboxExistsOnMaster
	snapshotNow             = time.Now
)

func snapshotIsTombstoned(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), StatusDeleted)
}

func snapshotRejectsNewUse(status string) bool {
	return snapshotIsTombstoned(status) || strings.EqualFold(strings.TrimSpace(status), StatusDeleting)
}

func wrapSnapshotNotFound(snapshotID string) error {
	return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotID)
}

// EnsureSnapshotReadyForNewUse gates create/rollback binding on a still-
// consumable snapshot. The read lock orders the READY check against a
// concurrent DeleteSnapshot / finalize write lock.
func EnsureSnapshotReadyForNewUse(ctx context.Context, snapshotID string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return ErrSnapshotNotFound
	}
	return withTemplateReadLock(snapshotResourceLockKey(snapshotID), func() error {
		rec, err := getSnapshotRecord(ctx, snapshotID)
		if err != nil {
			return err
		}
		if snapshotRejectsNewUse(rec.Status) {
			return wrapSnapshotNotFound(snapshotID)
		}
		if !strings.EqualFold(strings.TrimSpace(rec.Status), StatusReady) {
			return fmt.Errorf("%w: snapshot %s is in status %s", ErrTemplateAttemptInProgress, snapshotID, rec.Status)
		}
		return nil
	})
}

func tombstoneSnapshotRecord(ctx context.Context, snapshotID string) error {
	if err := updateSnapshotFields(ctx, snapshotID, map[string]any{
		"status":     StatusDeleted,
		"last_error": "",
	}); err != nil {
		return err
	}
	invalidateTemplateCaches(snapshotID)
	return nil
}

func syntheticTombstoneJobInfo(requestID, snapshotID string) *sandboxtypes.TemplateImageJobInfo {
	return &sandboxtypes.TemplateImageJobInfo{
		JobID:        firstNonEmpty(strings.TrimSpace(requestID), uuid.NewString()),
		RequestID:    strings.TrimSpace(requestID),
		TemplateID:   snapshotID,
		ResourceType: JobResourceTypeSnapshot,
		ResourceID:   snapshotID,
		Operation:    JobOperationSnapshotDelete,
		Status:       JobStatusReady,
		Phase:        JobPhaseReady,
		Progress:     100,
	}
}

func markSnapshotDeleteFailed(ctx context.Context, snapshotID, message string) error {
	return updateSnapshotFields(ctx, snapshotID, map[string]any{
		"status":     StatusDeleted,
		"last_error": message,
	})
}

func sandboxExistsOnMaster(ctx context.Context, sandboxID string) bool {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return false
	}
	if localcache.GetSandboxCache(sandboxID) != nil {
		return true
	}
	if _, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok {
		return true
	}
	if _, err := sandboxspec.Get(ctx, sandboxID); err == nil {
		return true
	} else if errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) {
		return false
	}
	// Spec store / DB error: fail closed so a blip cannot expire a live ref.
	return true
}

func listSnapshotRecordsByStatus(ctx context.Context, statuses ...string) ([]models.SnapshotRecord, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if len(statuses) == 0 {
		return nil, nil
	}
	var rows []models.SnapshotRecord
	if err := store.db.WithContext(ctx).Table(constants.SnapshotTableName).
		Where("status IN ?", statuses).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func expireOrphanSnapshotRuntimeRefs(ctx context.Context) error {
	rows, err := listSnapshotRecordsByStatus(ctx, StatusDeleted)
	if err != nil {
		return err
	}
	return expireOrphanSnapshotRuntimeRefsOn(ctx, rows)
}

// expireOrphanSnapshotRuntimeRefsOn detaches refs whose sandbox is gone from
// master AND whose last observation exceeds the TTL. Both are required: a
// live sandbox always keeps its ref, and the TTL guards against transient
// cache misses (S3 cleanup deactivates before unlinking, bypassing EBUSY).
func expireOrphanSnapshotRuntimeRefsOn(ctx context.Context, rows []models.SnapshotRecord) error {
	now := snapshotNow()
	var expireErr error
	for _, rec := range rows {
		refs, listErr := ListActiveSnapshotRuntimeRefs(ctx, rec.SnapshotID)
		if listErr != nil {
			expireErr = errors.Join(expireErr, listErr)
			continue
		}
		for _, ref := range refs {
			if sandboxExistsOnMasterFn(ctx, ref.SandboxID) {
				continue
			}
			observed := ref.AttachedAt
			if ref.LastSeenAt != nil && !ref.LastSeenAt.IsZero() {
				observed = *ref.LastSeenAt
			}
			if observed.IsZero() || now.Sub(observed) < snapshotOrphanRuntimeRefTTL {
				continue
			}
			if err := DetachSnapshotRuntimeBinding(ctx, ref.SandboxID, ref.BindingType, "orphan runtime ref expired"); err != nil {
				expireErr = errors.Join(expireErr, err)
			}
		}
	}
	return expireErr
}

// maybeFinalizeTombstone starts physical cleanup when a tombstoned snapshot
// has no remaining runtime refs. Execution is asynchronous so destroy hooks
// stay cheap; the 5min reconciler retries if the goroutine is lost.
func maybeFinalizeTombstone(ctx context.Context, snapshotID string) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return
	}
	jobID, err := beginTombstonePhysicalCleanup(ctx, snapshotID)
	if err != nil {
		log.G(ctx).Warnf("begin tombstone physical cleanup for %s failed: %v", snapshotID, err)
		return
	}
	if jobID == "" {
		return
	}
	go func() {
		jobCtx, cancel := synchronousSnapshotJobContext(context.Background(), "snapshot_tombstone_finalize", map[string]any{
			"job_id":      jobID,
			"snapshot_id": snapshotID,
		})
		defer cancel()
		info, err := GetTemplateImageJobInfo(jobCtx, jobID)
		if err != nil {
			log.G(jobCtx).Warnf("load tombstone delete job %s failed: %v", jobID, err)
			return
		}
		if _, err := executeSnapshotDeleteJob(jobCtx, info, snapshotID); err != nil {
			log.G(jobCtx).Warnf("tombstone physical cleanup for %s failed: %v", snapshotID, err)
		}
	}()
}

// beginTombstonePhysicalCleanup starts physical delete for a tombstoned
// snapshot with no remaining refs. Returns "" if gone, not tombstoned,
// still referenced, or already has an active job.
func beginTombstonePhysicalCleanup(ctx context.Context, snapshotID string) (string, error) {
	var jobID string
	err := withSnapshotWriteLocks([]string{snapshotResourceLockKey(snapshotID)}, func() error {
		rec, err := getSnapshotRecord(ctx, snapshotID)
		if err != nil {
			if errors.Is(err, ErrSnapshotNotFound) {
				return nil
			}
			return err
		}
		if !snapshotIsTombstoned(rec.Status) {
			return nil
		}
		n, err := countActiveSnapshotRuntimeRefsFn(ctx, snapshotID)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if _, err := getActiveSnapshotJobByResourceID(ctx, snapshotID); err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		id, err := insertSnapshotDeleteJob(ctx, snapshotTombstoneRequestID+uuid.NewString(), snapshotID, rec.OriginNodeID, rec.InstanceType)
		if err != nil {
			return err
		}
		jobID = id
		return nil
	})
	return jobID, err
}

func insertSnapshotDeleteJob(ctx context.Context, requestID, snapshotID, originNodeID, instanceType string) (string, error) {
	attemptNo, retryOfJobID, err := nextSnapshotAttempt(ctx, snapshotID)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(snapshotDeleteJobRequest{
		RequestID:  requestID,
		SnapshotID: snapshotID,
		NodeID:     originNodeID,
	})
	if err != nil {
		return "", err
	}
	jobID := uuid.NewString()
	record := &models.TemplateImageJob{
		JobID:        jobID,
		TemplateID:   snapshotID,
		RequestID:    requestID,
		ResourceType: JobResourceTypeSnapshot,
		ResourceID:   snapshotID,
		AttemptNo:    attemptNo,
		RetryOfJobID: retryOfJobID,
		Operation:    JobOperationSnapshotDelete,
		NodeID:       originNodeID,
		InstanceType: instanceType,
		Status:       JobStatusPending,
		Phase:        JobPhaseDeleting,
		Progress:     0,
		RequestJSON:  string(payload),
	}
	if err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(constants.SnapshotTableName).
			Where("snapshot_id = ?", snapshotID).
			Updates(map[string]any{
				"status":     StatusDeleting,
				"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
			}).Error; err != nil {
			return err
		}
		return tx.Table(constants.TemplateImageJobTableName).Create(record).Error
	}); err != nil {
		return "", err
	}
	return jobID, nil
}

// reconcileSnapshotTombstones expires orphan refs and finalizes drained
// tombstones, sharing one list of DELETED rows.
func reconcileSnapshotTombstones(ctx context.Context) error {
	rows, err := listSnapshotRecordsByStatus(ctx, StatusDeleted)
	if err != nil {
		return err
	}
	expireErr := expireOrphanSnapshotRuntimeRefsOn(ctx, rows)
	if failErr := failStaleDeleteJobsOnTombstones(ctx, rows); failErr != nil {
		return errors.Join(expireErr, failErr)
	}
	for _, rec := range rows {
		maybeFinalizeTombstone(ctx, rec.SnapshotID)
	}
	return expireErr
}

// failStaleDeleteJobsOnTombstones marks a leftover PENDING/RUNNING
// SNAPSHOT_DELETE job FAILED when the row is already DELETED and the job
// is past the operation timeout. That split state blocks finalize forever.
func failStaleDeleteJobsOnTombstones(ctx context.Context, rows []models.SnapshotRecord) error {
	var failErr error
	for _, rec := range rows {
		if err := failStaleDeleteJobOnTombstone(ctx, rec); err != nil {
			failErr = errors.Join(failErr, err)
		}
	}
	return failErr
}

func failStaleDeleteJobOnTombstone(ctx context.Context, rec models.SnapshotRecord) error {
	active, err := getActiveSnapshotJobByResourceID(ctx, rec.SnapshotID)
	if err != nil && !errorsIsRecordNotFound(err) {
		return err
	}
	if active == nil || !shouldFailStaleDeleteJobOnTombstone(rec, active) {
		return nil
	}
	return updateTemplateImageJob(ctx, active.JobID, map[string]any{
		"status":        JobStatusFailed,
		"error_message": fmt.Sprintf("stale snapshot delete job %s exceeded %s on tombstone", active.JobID, snapshotOperationTimeout),
	})
}

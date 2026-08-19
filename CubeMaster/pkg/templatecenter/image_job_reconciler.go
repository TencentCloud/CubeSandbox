// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"gorm.io/gorm"
)

// Why this reconciler exists
//
// A remote-mode job goes PENDING -> RUNNING -> BUILT -> (resume) -> READY.
// BUILT means CubeTemplateCenter finished the ext4 and CubeMaster's callback
// handler kicked off ResumeTemplateImageJobAfterRemoteBuild in a goroutine.
// That goroutine is in-memory state, so it is lost whenever CubeMaster
// restarts -- including the restart that happens when an operator flips
// templatecenter_enabled back to false.
//
// Without a sweep the job would sit at BUILT forever: no timeout, no error,
// and a client polling for READY/FAILED would poll indefinitely. This
// reconciler closes that hole, and deliberately RETRIES the resume before
// giving up: the ext4 already exists on the shared artifact disk, so the work
// is recoverable. Every step of the resume path is idempotent
// (claimRootfsArtifactForBuild takes FOR UPDATE, finalize overwrites,
// distribution skips already-READY nodes, UpsertReplica upserts), which is
// what makes replaying it safe.
//
// It also sweeps PENDING/RUNNING jobs, for the same reason applied to local
// mode: runTemplateImageJob is an in-memory goroutine too. CubeTemplateCenter
// has its own sweep for these, but it cannot be relied upon here -- after a
// switch back to local mode TC may well be shut down.

const (
	// Timings deliberately REUSE CubeMaster's existing reconcile constants
	// instead of introducing a third set of thresholds. Keeping one source of
	// truth per concept avoids the situation where the same kind of sweep runs
	// on three different cadences and nobody knows which one applies.
	//
	// imageJobReconcileInterval: same cadence as the snapshot reconciler.
	imageJobReconcileInterval = snapshotReconcilerInterval
	// builtStaleAfter: an abandoned resume is just a stalled async operation,
	// so it reuses the operation timeout the snapshot reconciler already uses.
	// It only has to be comfortably longer than a normal resume (seconds to a
	// couple of minutes, dominated by artifact distribution) so the sweep never
	// races a live one.
	builtStaleAfter = snapshotOperationTimeout
	// runningStaleAfter MUST stay equal to
	// CubeTemplateCenter/pkg/reconcile.defaultStaleAfter -- both sweeps look at
	// the same rows, and different thresholds would mean whichever process
	// happened to be running decided the outcome. Change both together.
	runningStaleAfter = 2 * time.Hour
	// imageJobReconcileBatch matches artifactGCMaxPerPass: same reasoning, keep
	// one pass from holding the advisory lock for an unbounded time.
	imageJobReconcileBatch = artifactGCMaxPerPass
	// imageJobReconcileLock is intentionally distinct from the artifact-GC and
	// schema-migration locks: those protect unrelated work and must not
	// serialize against this sweep.
	imageJobReconcileLock = "cubemaster_templatecenter_image_job_reconcile_v1"

	envImageJobReconcileDisabled = "CUBE_MASTER_IMAGE_JOB_RECONCILE_DISABLED"
	// envBuiltStaleAfter exists to shorten the wait in tests (e.g. "1m").
	// Production should leave it unset and inherit builtStaleAfter.
	envBuiltStaleAfter = "CUBE_MASTER_BUILT_STALE_AFTER"
)

var imageJobReconcileOnce sync.Once

// startImageJobReconciler launches the stuck-job sweep. Safe to call in any
// build mode: it converges jobs left behind by an earlier mode, which is
// exactly what makes switching modes safe.
func startImageJobReconciler(ctx context.Context) {
	if imageJobReconcileDisabled() {
		log.G(ctx).Warnf("image job reconciler disabled by %s", envImageJobReconcileDisabled)
		return
	}
	imageJobReconcileOnce.Do(func() {
		go func() {
			runImageJobReconcilePass(detachTemplateImageJobContext(ctx, "image_job_reconcile", nil))
			ticker := time.NewTicker(imageJobReconcileInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runImageJobReconcilePass(detachTemplateImageJobContext(ctx, "image_job_reconcile", nil))
				}
			}
		}()
	})
}

func imageJobReconcileDisabled() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(envImageJobReconcileDisabled)))
	return err == nil && v
}

func builtStaleThreshold() time.Duration {
	raw := strings.TrimSpace(os.Getenv(envBuiltStaleAfter))
	if raw == "" {
		return builtStaleAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return builtStaleAfter
	}
	return d
}

// runImageJobReconcilePass performs one sweep under the advisory lock.
func runImageJobReconcilePass(ctx context.Context) {
	if !isReady() {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})

	if err := withImageJobReconcileLock(ctx, func() error {
		if err := resumeStuckBuiltJobs(ctx); err != nil {
			logger.Errorf("resume stuck BUILT jobs: %v", err)
		}
		if err := failStaleRunningJobs(ctx); err != nil {
			logger.Errorf("fail stale RUNNING jobs: %v", err)
		}
		return nil
	}); err != nil {
		logger.Errorf("image job reconcile pass: %v", err)
	}
}

// withImageJobReconcileLock runs fn while holding a cross-instance advisory
// lock, so only one CubeMaster replica sweeps per tick. Mirrors the artifact-GC
// locking discipline, including discarding the pinned connection whenever the
// lock state becomes unknown.
func withImageJobReconcileLock(ctx context.Context, fn func() error) (retErr error) {
	conn, err := store.db.WithContext(ctx).DB()
	if err != nil {
		return fmt.Errorf("resolve sql db: %w", err)
	}
	sqlConn, err := conn.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	defer func() {
		if closeErr := sqlConn.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close pinned connection: %w", closeErr))
		}
	}()

	sess := store.db.Session(&gorm.Session{NewDB: true})
	sess.Error = nil
	sess = sess.WithContext(ctx)
	sess.Statement.ConnPool = sqlConn

	locked, err := trySessionLock(sess, imageJobReconcileLock)
	if err != nil {
		return errors.Join(fmt.Errorf("acquire lock: %w", err), discardPinnedSession(sess))
	}
	if !locked {
		// Another replica is sweeping; nothing to do this tick.
		return nil
	}
	defer func() {
		released, releaseErr := releaseSessionLock(sess, imageJobReconcileLock)
		if releaseErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release lock: %w", releaseErr), discardPinnedSession(sess))
			return
		}
		if !released {
			log.G(ctx).Warnf("image job reconcile lock %q was not held at release time", imageJobReconcileLock)
		}
	}()

	return fn()
}

// stuckImageJob is the projection the sweep needs.
type stuckImageJob struct {
	JobID      string    `gorm:"column:job_id"`
	TemplateID string    `gorm:"column:template_id"`
	Status     string    `gorm:"column:status"`
	Phase      string    `gorm:"column:phase"`
	ResultJSON string    `gorm:"column:result_json"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// resumeStuckBuiltJobs replays the post-build pipeline for jobs abandoned at
// BUILT, and fails the ones that cannot be replayed.
func resumeStuckBuiltJobs(ctx context.Context) error {
	threshold := builtStaleThreshold()
	cutoff := time.Now().Add(-threshold)

	var jobs []stuckImageJob
	if err := store.db.WithContext(ctx).
		Table(constants.TemplateImageJobTableName).
		Select("job_id, template_id, status, phase, result_json, updated_at").
		Where("status = ?", JobStatusBuilt).
		Where("updated_at < ?", cutoff).
		Limit(imageJobReconcileBatch).
		Find(&jobs).Error; err != nil {
		return fmt.Errorf("scan BUILT jobs: %w", err)
	}
	if len(jobs) == 0 {
		return nil
	}

	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	logger.Warnf("found %d job(s) stuck at BUILT for over %s; replaying the post-build pipeline", len(jobs), threshold)

	for _, job := range jobs {
		result, err := remoteBuildResultFromResultJSON(job.ResultJSON)
		if err != nil {
			// Nothing to replay from. Fail with an actionable message instead
			// of leaving the job stuck: the client sees why and can retry.
			msg := fmt.Sprintf(
				"build finished but the post-build step was lost (likely a CubeMaster restart, "+
					"e.g. a templatecenter_enabled switch) and cannot be replayed: %v. retry the build", err)
			logger.Errorf("job %s: %s", job.JobID, msg)
			if updErr := updateTemplateImageJob(ctx, job.JobID, map[string]any{
				"status":        JobStatusFailed,
				"progress":      100,
				"error_message": msg,
			}); updErr != nil {
				logger.Errorf("job %s: mark FAILED: %v", job.JobID, updErr)
			}
			continue
		}

		logger.Warnf("job %s: replaying post-build pipeline (artifact %s)", job.JobID, result.ArtifactID)
		if err := ResumeTemplateImageJobAfterRemoteBuild(ctx, job.JobID, result); err != nil {
			// ResumeTemplateImageJobAfterRemoteBuild already wrote the failure
			// into the job row, so the client always has a concrete reason.
			logger.Errorf("job %s: replay failed: %v", job.JobID, err)
			continue
		}
		logger.Infof("job %s: replay succeeded", job.JobID)
	}
	return nil
}

// remoteBuildResultFromResultJSON rebuilds the artifact metadata from the
// terminal callback payload the handler stored in result_json. This is the
// reason the raw payload is persisted there.
func remoteBuildResultFromResultJSON(payload string) (*RemoteBuildResult, error) {
	if strings.TrimSpace(payload) == "" {
		return nil, errors.New("result_json is empty")
	}
	var raw struct {
		ArtifactID              string `json:"artifact_id"`
		TemplateSpecFingerprint string `json:"template_spec_fingerprint"`
		SourceImageDigest       string `json:"source_image_digest"`
		Ext4Path                string `json:"ext4_path"`
		Ext4SHA256              string `json:"ext4_sha256"`
		Ext4SizeBytes           int64  `json:"ext4_size_bytes"`
		ImageConfigJSON         string `json:"image_config_json"`
		MasterNodeIP            string `json:"master_node_ip"`
		CubeEgressCABaked       bool   `json:"cube_egress_ca_baked"`
		CubeEgressCAFingerprint string `json:"cube_egress_ca_fingerprint"`
		CubeEgressCATargets     int    `json:"cube_egress_ca_targets_written"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("decode result_json: %w", err)
	}
	result := &RemoteBuildResult{
		ArtifactID:              raw.ArtifactID,
		TemplateSpecFingerprint: raw.TemplateSpecFingerprint,
		SourceImageDigest:       raw.SourceImageDigest,
		Ext4Path:                raw.Ext4Path,
		Ext4SHA256:              raw.Ext4SHA256,
		Ext4SizeBytes:           raw.Ext4SizeBytes,
		ImageConfigJSON:         raw.ImageConfigJSON,
		MasterNodeIP:            raw.MasterNodeIP,
		CubeEgressCABaked:       raw.CubeEgressCABaked,
		CubeEgressCAFingerprint: raw.CubeEgressCAFingerprint,
		CubeEgressCATargets:     raw.CubeEgressCATargets,
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	// The ext4 must still be on disk. CubeMaster and CubeTemplateCenter share
	// the artifact directory (design §9.7), so this holds even if TC has since
	// been shut down -- but not if GC already reclaimed the artifact.
	if _, err := os.Stat(result.Ext4Path); err != nil {
		return nil, fmt.Errorf("artifact file %s is gone: %w", result.Ext4Path, err)
	}
	return result, nil
}

// failStaleRunningJobs marks PENDING/RUNNING jobs with no progress for
// runningStaleAfter as FAILED. Overlaps with CubeTemplateCenter's own sweep on
// purpose: the update is guarded by the same status predicate, so whichever
// runs second is a no-op, and this one still works when TC is offline.
func failStaleRunningJobs(ctx context.Context) error {
	cutoff := time.Now().Add(-runningStaleAfter)

	var jobs []stuckImageJob
	if err := store.db.WithContext(ctx).
		Table(constants.TemplateImageJobTableName).
		Select("job_id, template_id, status, phase, updated_at").
		Where("status IN ?", []string{JobStatusPending, JobStatusRunning}).
		Where("updated_at < ?", cutoff).
		Limit(imageJobReconcileBatch).
		Find(&jobs).Error; err != nil {
		return fmt.Errorf("scan stale running jobs: %w", err)
	}
	if len(jobs) == 0 {
		return nil
	}

	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	for _, job := range jobs {
		msg := fmt.Sprintf(
			"build was interrupted: no progress for over %s (last update %s, phase %s). "+
				"the building instance was likely restarted. retry the build",
			runningStaleAfter, job.UpdatedAt.Format(time.RFC3339), job.Phase)

		tx := store.db.WithContext(ctx).
			Table(constants.TemplateImageJobTableName).
			Where("job_id = ?", job.JobID).
			Where("status IN ?", []string{JobStatusPending, JobStatusRunning}).
			Updates(map[string]any{
				"status":        JobStatusFailed,
				"progress":      100,
				"error_message": msg,
				"updated_at":    time.Now(),
			})
		if tx.Error != nil {
			logger.Errorf("job %s: mark stale FAILED: %v", job.JobID, tx.Error)
			continue
		}
		if tx.RowsAffected > 0 {
			logger.Warnf("job %s (template %s): marked FAILED after %s without progress",
				job.JobID, job.TemplateID, runningStaleAfter)
		}
	}
	return nil
}

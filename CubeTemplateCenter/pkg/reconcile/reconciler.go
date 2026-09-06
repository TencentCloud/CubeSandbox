// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package reconcile implements the background sweep that rescues template
// build jobs abandoned mid-flight.
//
// Why this is mandatory, not optional (design §7.3):
//   - TC is killed (OOM / rolling upgrade / node failure) while building:
//     the job row stays RUNNING forever and the client polls forever.
//   - The status callback to CubeMaster is lost after the retry budget in
//     pkg/build.Reporter is exhausted: same outcome.
//
// The sweep uses updated_at rather than an owner heartbeat because the job
// table has no owner column (design §9.4). Progress reports refresh
// updated_at continuously during a healthy build, so "not updated for a long
// time" is a reliable liveness signal.
//
// Mutual exclusion across replicas uses the DB session lock (pkg/lock), so
// only one replica sweeps per tick and no leader election is needed (§9.3).
//
// TODO(templatecenter): no unit tests yet for failStaleJobs (the guard-write
// predicate and the RowsAffected==0 "converged before update" skip path in
// particular). Track alongside the pkg/lock test gap.
package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/lock"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	"gorm.io/gorm"
)

const (
	// defaultInterval is the sweep cadence (design §7.3: 10 minutes).
	defaultInterval = 10 * time.Minute

	// Staleness thresholds are per (status, phase) because different stages
	// have wildly different expected durations:
	//
	//   - PENDING: TC never picked up the job. If it's still PENDING after 10
	//     minutes, the Master->TC forwarding likely failed or TC was down at
	//     submit time. Fail fast so the client can retry.
	//   - RUNNING + PULLING: image pull is network-bound and should not take
	//     more than 30 minutes even for multi-GB images on slow links.
	//   - RUNNING + other phases (UNPACKING / BUILDING_EXT4 / DISTRIBUTING /
	//     CREATING_TEMPLATE): ext4 mkfs and distribution can legitimately take
	//     up to an hour for very large rootfs, but not more.
	//
	// These thresholds MUST stay in sync with
	// CubeMaster/pkg/templatecenter.runningStaleAfter (which uses the same
	// tiered logic): both sweeps scan the same rows, so different thresholds
	// would mean whichever process happened to run decided the outcome.
	defaultPendingStaleAfter  = 10 * time.Minute
	defaultPullingStaleAfter  = 30 * time.Minute
	defaultRunningStaleAfter  = 1 * time.Hour

	// cleanupPendingGrace is how long an artifact row marked CLEANUP_PENDING
	// must age before the reconciler deletes it as a backstop. The primary
	// delete path is immediate (CubeMaster calls the delete endpoint right
	// after marking the row), so this grace only fires when CubeMaster crashed
	// between marking and notifying. 5 minutes is comfortably longer than the
	// immediate-delete round trip.
	cleanupPendingGrace = 5 * time.Minute

	// maxRowsPerSweep bounds a single sweep so a large backlog cannot hold the
	// session lock for an unbounded time.
	maxRowsPerSweep = 200
)

// ArtifactDeleter is the subset of build.ArtifactDeleter the reconciler needs
// for the CLEANUP_PENDING backstop sweep. Declared as an interface so the
// reconciler does not import pkg/build (which would be a package cycle:
// build already imports pkg/reconcile transitively via lock).
type ArtifactDeleter interface {
	Delete(ctx context.Context, artifactID string) error
}

// Reconciler periodically fails jobs that were abandoned mid-build and sweeps
// artifacts marked CLEANUP_PENDING whose immediate delete never arrived.
type Reconciler struct {
	db       *gorm.DB
	interval time.Duration
	// Tiered staleness thresholds (see constants above).
	pendingStaleAfter time.Duration
	pullingStaleAfter time.Duration
	runningStaleAfter time.Duration
	// deleter removes artifact data for CLEANUP_PENDING rows. May be nil, in
	// which case the backstop sweep is skipped (immediate deletes still work).
	deleter ArtifactDeleter
}

// New builds a Reconciler over db. Cadence and staleness thresholds can be
// overridden with CUBE_TEMPLATE_CENTER_RECONCILE_INTERVAL /
// CUBE_TEMPLATE_CENTER_RECONCILE_PENDING_STALE_AFTER /
// CUBE_TEMPLATE_CENTER_RECONCILE_PULLING_STALE_AFTER /
// CUBE_TEMPLATE_CENTER_RECONCILE_RUNNING_STALE_AFTER (Go duration strings,
// e.g. "5m", "30m"). The pre-rename CUBE_TC_* spellings still work; see
// pkg/tcconfig.
func New(db *gorm.DB) *Reconciler {
	intervalRaw, intervalSet := tcconfig.ReconcileInterval()
	pendingRaw, pendingSet := tcconfig.ReconcilePendingStaleAfter()
	pullingRaw, pullingSet := tcconfig.ReconcilePullingStaleAfter()
	runningRaw, runningSet := tcconfig.ReconcileRunningStaleAfter()
	return &Reconciler{
		db:                db,
		interval:          durationOverride(intervalRaw, intervalSet, defaultInterval),
		pendingStaleAfter: durationOverride(pendingRaw, pendingSet, defaultPendingStaleAfter),
		pullingStaleAfter: durationOverride(pullingRaw, pullingSet, defaultPullingStaleAfter),
		runningStaleAfter: durationOverride(runningRaw, runningSet, defaultRunningStaleAfter),
	}
}

// SetDeleter installs the artifact deleter used by the CLEANUP_PENDING
// backstop sweep. Called once at startup; optional (nil disables the sweep).
func (r *Reconciler) SetDeleter(d ArtifactDeleter) {
	r.deleter = d
}

// Disabled reports whether the operator turned the sweep off.
func Disabled() bool {
	return tcconfig.ReconcileDisabled()
}

// Run blocks until ctx is canceled, sweeping every interval. Intended to be
// launched in a supervised goroutine at startup.
func (r *Reconciler) Run(ctx context.Context) {
	if r.db == nil {
		log.G(ctx).Errorf("reconciler not started: db handle is nil")
		return
	}
	log.G(ctx).Infof("templatecenter reconciler started: interval=%s pending_stale=%s pulling_stale=%s running_stale=%s",
		r.interval, r.pendingStaleAfter, r.pullingStaleAfter, r.runningStaleAfter)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.G(ctx).Infof("templatecenter reconciler stopped")
			return
		case <-ticker.C:
			if err := r.SweepOnce(ctx); err != nil {
				log.G(ctx).Errorf("reconciler sweep fail: %v", err)
			}
		}
	}
}

// SweepOnce runs a single sweep under the cross-replica session lock. It is
// exported so operators can trigger it from a test harness.
func (r *Reconciler) SweepOnce(ctx context.Context) error {
	return lock.WithReconcileLock(ctx, r.db, func() error {
		if err := r.failStaleJobs(ctx); err != nil {
			return err
		}
		return r.cleanupPendingArtifacts(ctx)
	})
}

// cleanupPendingArtifacts deletes artifact rows that CubeMaster marked
// CLEANUP_PENDING more than cleanupPendingGrace ago but whose immediate
// delete never arrived (CubeMaster crashed between marking and notifying).
// The immediate delete path (POST /tc/api/v1/artifact/delete) handles the
// common case; this sweep is purely the backstop.
//
// Runs under the same session lock as failStaleJobs so exactly one replica
// sweeps, and is skipped entirely when no deleter is installed.
func (r *Reconciler) cleanupPendingArtifacts(ctx context.Context) error {
	if r.deleter == nil {
		return nil
	}
	cutoff := time.Now().Add(-cleanupPendingGrace)
	var rows []struct {
		ArtifactID string `gorm:"column:artifact_id"`
	}
	if err := r.db.WithContext(ctx).
		Table(constants.RootfsArtifactTableName).
		Select("artifact_id").
		Where("status = ?", "CLEANUP_PENDING").
		Where("updated_at < ?", cutoff).
		Limit(maxRowsPerSweep).
		Find(&rows).Error; err != nil {
		return fmt.Errorf("scan cleanup-pending artifacts: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	log.G(ctx).Warnf("reconciler: found %d cleanup-pending artifact(s) to delete (backstop)", len(rows))
	for _, row := range rows {
		if err := r.deleter.Delete(ctx, row.ArtifactID); err != nil {
			log.G(ctx).Warnf("reconciler: backstop delete artifact %s fail: %v", row.ArtifactID, err)
		}
	}
	return nil
}

// staleJob is the projection needed to decide and report.
type staleJob struct {
	JobID      string    `gorm:"column:job_id"`
	TemplateID string    `gorm:"column:template_id"`
	Status     string    `gorm:"column:status"`
	Phase      string    `gorm:"column:phase"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// failStaleJobs marks PENDING/RUNNING jobs whose updated_at is older than
// their tier-specific staleness threshold as FAILED, so clients stop polling
// and the template can be rebuilt.
//
// BUILT is deliberately NOT swept: a BUILT job has a usable artifact and is
// waiting on the distribution resume step, which is a different (and
// recoverable) condition — failing it would throw away a finished build.
//
// Tiered thresholds (see constants):
//   - PENDING: 10 minutes (TC never picked up the job)
//   - RUNNING + PULLING: 30 minutes (image pull is network-bound)
//   - RUNNING + other phases: 1 hour (ext4 mkfs / distribution can be slow)
func (r *Reconciler) failStaleJobs(ctx context.Context) error {
	// Scan all PENDING/RUNNING jobs; we'll apply tier-specific thresholds
	// in-memory rather than in SQL to keep the query simple and avoid
	// multiple round-trips.
	var jobs []staleJob
	if err := r.db.WithContext(ctx).
		Table(constants.TemplateImageJobTableName).
		Select("job_id, template_id, status, phase, updated_at").
		Where("status IN ?", []string{templatecenter.JobStatusPending, templatecenter.JobStatusRunning}).
		Where("updated_at < ?", time.Now().Add(-r.pendingStaleAfter)). // Earliest possible cutoff
		Limit(maxRowsPerSweep).
		Find(&jobs).Error; err != nil {
		return fmt.Errorf("scan stale jobs: %w", err)
	}

	if len(jobs) == 0 {
		log.G(ctx).Debugf("reconciler: no stale jobs")
		return nil
	}

	var failures int
	for _, job := range jobs {
		staleAfter := r.staleThresholdFor(job.Status, job.Phase)
		if staleAfter == 0 {
			// Job is not stale under its tier threshold; skip.
			continue
		}
		cutoff := time.Now().Add(-staleAfter)
		if job.UpdatedAt.After(cutoff) {
			// Job is recent enough under its tier threshold; skip.
			continue
		}

		msg := r.staleMessage(job, staleAfter)

		// Guard the write with the same status predicate so a job that
		// finished between the scan and this update is left alone.
		tx := r.db.WithContext(ctx).
			Table(constants.TemplateImageJobTableName).
			Where("job_id = ?", job.JobID).
			Where("status IN ?", []string{templatecenter.JobStatusPending, templatecenter.JobStatusRunning}).
			Updates(map[string]any{
				"status":        templatecenter.JobStatusFailed,
				"progress":      100,
				"error_message": msg,
				"updated_at":    time.Now(),
			})
		if tx.Error != nil {
			failures++
			log.G(ctx).Errorf("reconciler: fail stale job %s: %v", job.JobID, tx.Error)
			continue
		}
		if tx.RowsAffected == 0 {
			log.G(ctx).Infof("reconciler: job %s converged before update, skipped", job.JobID)
			continue
		}
		log.G(ctx).Warnf("reconciler: marked job %s (template %s) FAILED after %s without progress (status=%s phase=%s)",
			job.JobID, job.TemplateID, staleAfter, job.Status, job.Phase)
	}

	if failures > 0 {
		return fmt.Errorf("failed to update %d/%d stale jobs", failures, len(jobs))
	}
	return nil
}

// staleThresholdFor returns the staleness threshold for a given (status, phase)
// combination, or 0 if the job should never be considered stale.
func (r *Reconciler) staleThresholdFor(status, phase string) time.Duration {
	switch status {
	case templatecenter.JobStatusPending:
		return r.pendingStaleAfter
	case templatecenter.JobStatusRunning:
		if phase == templatecenter.JobPhasePulling {
			return r.pullingStaleAfter
		}
		return r.runningStaleAfter
	default:
		return 0 // BUILT / READY / FAILED are not swept
	}
}

// staleMessage constructs a tier-specific error message for a stale job.
func (r *Reconciler) staleMessage(job staleJob, staleAfter time.Duration) string {
	switch job.Status {
	case templatecenter.JobStatusPending:
		return fmt.Sprintf(
			"job was never picked up by template center (no progress for over %s, last update %s); "+
				"the Master->TC forwarding likely failed or template center was down at submit time. "+
				"Check cubemaster logs for forwarding errors, then retry the build",
			staleAfter, job.UpdatedAt.Format(time.RFC3339))
	case templatecenter.JobStatusRunning:
		if job.Phase == templatecenter.JobPhasePulling {
			return fmt.Sprintf(
				"image pull stalled (no progress for over %s, last update %s); "+
					"the registry may be unreachable or the image may be very large. "+
					"Check template center logs around %s for pull errors, then retry the build",
				staleAfter, job.UpdatedAt.Format(time.RFC3339), job.UpdatedAt.Format(time.RFC3339))
		}
		return fmt.Sprintf(
			"build was interrupted at phase %s (no progress for over %s, last update %s); "+
				"the template center instance was likely restarted. "+
				"Check template center logs around %s for the root cause, then retry the build",
			job.Phase, staleAfter, job.UpdatedAt.Format(time.RFC3339), job.UpdatedAt.Format(time.RFC3339))
	default:
		return fmt.Sprintf("job in unexpected state %s/%s marked failed after %s", job.Status, job.Phase, staleAfter)
	}
}

// durationOverride applies a raw Go duration override, falling back when it is
// absent, unparseable, or non-positive.
//
// Environment resolution — including the legacy-name fallback — belongs to
// tcconfig; this function owns only the default and the sanity bound.
func durationOverride(raw string, found bool, fallback time.Duration) time.Duration {
	if !found {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

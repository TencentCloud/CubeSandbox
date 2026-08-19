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
	// defaultStaleAfter is how long a RUNNING job may go without an update
	// before it is considered abandoned. Design §7.3 recommends 2x the longest
	// expected build; 2 hours is the conservative default. Pulling a very large
	// image can leave long gaps between progress reports, so do not shrink this
	// without measuring.
	//
	// MUST stay equal to CubeMaster/pkg/templatecenter.runningStaleAfter: both
	// sweeps scan the same rows, so different thresholds would mean whichever
	// process happened to run decided the outcome. Change both together.
	defaultStaleAfter = 2 * time.Hour
	// maxRowsPerSweep bounds a single sweep so a large backlog cannot hold the
	// session lock for an unbounded time.
	maxRowsPerSweep = 200
)

// Reconciler periodically fails jobs that were abandoned mid-build.
type Reconciler struct {
	db         *gorm.DB
	interval   time.Duration
	staleAfter time.Duration
}

// New builds a Reconciler over db. Cadence and staleness threshold can be
// overridden with CUBE_TEMPLATE_CENTER_RECONCILE_INTERVAL /
// CUBE_TEMPLATE_CENTER_RECONCILE_STALE_AFTER (Go duration strings, e.g. "5m",
// "30m"). The pre-rename CUBE_TC_* spellings still work; see pkg/tcconfig.
func New(db *gorm.DB) *Reconciler {
	intervalRaw, intervalSet := tcconfig.ReconcileInterval()
	staleRaw, staleSet := tcconfig.ReconcileStaleAfter()
	return &Reconciler{
		db:         db,
		interval:   durationOverride(intervalRaw, intervalSet, defaultInterval),
		staleAfter: durationOverride(staleRaw, staleSet, defaultStaleAfter),
	}
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
	log.G(ctx).Infof("templatecenter reconciler started: interval=%s stale_after=%s", r.interval, r.staleAfter)

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
		return r.failStaleJobs(ctx)
	})
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
// staleAfter as FAILED, so clients stop polling and the template can be
// rebuilt.
//
// BUILT is deliberately NOT swept: a BUILT job has a usable artifact and is
// waiting on the distribution resume step, which is a different (and
// recoverable) condition — failing it would throw away a finished build.
func (r *Reconciler) failStaleJobs(ctx context.Context) error {
	cutoff := time.Now().Add(-r.staleAfter)

	var jobs []staleJob
	if err := r.db.WithContext(ctx).
		Table(constants.TemplateImageJobTableName).
		Select("job_id, template_id, status, phase, updated_at").
		Where("status IN ?", []string{templatecenter.JobStatusPending, templatecenter.JobStatusRunning}).
		Where("updated_at < ?", cutoff).
		Limit(maxRowsPerSweep).
		Find(&jobs).Error; err != nil {
		return fmt.Errorf("scan stale jobs: %w", err)
	}

	if len(jobs) == 0 {
		log.G(ctx).Debugf("reconciler: no stale jobs (cutoff=%s)", cutoff.Format(time.RFC3339))
		return nil
	}

	log.G(ctx).Warnf("reconciler: found %d stale job(s) older than %s", len(jobs), r.staleAfter)

	var failures int
	for _, job := range jobs {
		msg := fmt.Sprintf(
			"build was interrupted: no progress for over %s (last update %s, phase %s); "+
				"the template center instance was likely restarted. retry the build",
			r.staleAfter, job.UpdatedAt.Format(time.RFC3339), job.Phase)

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
		log.G(ctx).Warnf("reconciler: marked job %s (template %s) FAILED after %s without progress",
			job.JobID, job.TemplateID, r.staleAfter)
	}

	if failures > 0 {
		return fmt.Errorf("failed to update %d/%d stale jobs", failures, len(jobs))
	}
	return nil
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package tombstone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	defaultRetention  = 168 * time.Hour // 7 days
	defaultInterval   = time.Hour
	defaultBatchSize  = 500
	defaultMaxPerPass = 5000
	maxBatchSize      = 5000 // hard cap so a misconfigured batch_size can't build a giant IN (...) statement
	minRetention      = time.Hour
	minInterval       = time.Minute

	// passTimeout bounds a single purge pass so a slow DB cannot hold the
	// advisory lock indefinitely; work resumes on the next tick.
	passTimeout        = 5 * time.Minute
	lockReleaseTimeout = 5 * time.Second
)

// Config controls one component's tombstone purger.
type Config struct {
	// Enabled gates the purger. It is a plain bool with zero value false:
	// the package does NOT default it on — each caller decides. The two
	// production call sites (CubeMaster/CubeOps main) also default it to FALSE
	// (the purge is irreversible, so it is strictly opt-in via config).
	Enabled bool
	// DryRun selects candidate rows and logs counts but issues no DELETE.
	// Use it for a safe first rollout against a large existing backlog.
	DryRun bool
	// Tables is the trusted, caller-owned list of tombstone tables to purge.
	// GORM quotes each name per dialect, but identifiers are never
	// parameterised — so these MUST be compile-time constants, never user input.
	Tables []string
	// TablesFn, if non-nil, resolves the table list at the START of each pass, so
	// the purge set can track live config that changes after Start (e.g. CubeMaster's
	// disable_hard_delete, which the app reads live on every delete). nil => use the
	// static Tables list.
	TablesFn func() []string
	// Retention: rows with deleted_at < now-Retention are eligible. Clamped to
	// [minRetention, +inf); <=0 → defaultRetention. A non-positive retention
	// would otherwise make cutoff>=now and purge seconds-old tombstones.
	Retention time.Duration
	// Interval between passes. Clamped to [minInterval, +inf); <=0 → default.
	Interval time.Duration
	// BatchSize rows are deleted per statement within a pass.
	BatchSize int
	// MaxPerPass is the hard cap on rows purged per table per tick, so a large
	// backlog drains across many ticks instead of one long, lock-holding pass
	// (MySQL binlog/replica-lag, PG dead-tuple bloat).
	MaxPerPass int
	// LockName is the advisory-lock name. Make it per-component (e.g.
	// "cubemaster_tombstone_purge_v1"); HA replicas of the same component share
	// it so only one runs per tick.
	LockName string
	// OnPurge, if non-nil, is invoked once per table per pass. It runs while the
	// cluster advisory lock is held, so it MUST be fast — a slow/blocking
	// callback directly extends the lock hold across the HA cluster. It is with the number of
	// rows purged (or would-be-purged under DryRun) and any per-table error. It
	// lets a binary record into its own metrics backend without this package
	// importing it.
	OnPurge func(table string, purged int, err error)
}

// sanitized returns cfg with safe defaults and clamps applied. Idempotent.
func (c Config) sanitized() Config {
	out := c
	switch {
	case out.Retention <= 0:
		out.Retention = defaultRetention
	case out.Retention < minRetention:
		out.Retention = minRetention
	}
	switch {
	case out.Interval <= 0:
		out.Interval = defaultInterval
	case out.Interval < minInterval:
		out.Interval = minInterval
	}
	if out.BatchSize <= 0 {
		out.BatchSize = defaultBatchSize
	}
	if out.BatchSize > maxBatchSize {
		out.BatchSize = maxBatchSize
	}
	if out.MaxPerPass <= 0 {
		out.MaxPerPass = defaultMaxPerPass
	}
	if out.BatchSize > out.MaxPerPass {
		out.BatchSize = out.MaxPerPass
	}
	return out
}

// startOnce deduplicates Start per lock name so repeated calls (or accidental
// double-registration) never launch two goroutines for the same component.
var startOnce sync.Map // map[lockName]*sync.Once

// runPassFn is the runPass entry point used by run(); it is a variable so tests
// can inject faults (e.g. a panic) without a live database. Mirrors the
// cleanupArtifactFullyGC seam in CubeMaster's artifact_gc.go.
var runPassFn = runPass

// Start launches (at most once per LockName) a background goroutine that
// periodically hard-purges tombstoned rows older than Retention. It returns
// immediately and is safe to call from init paths. The goroutine stops when
// ctx is cancelled.
//
// Start is a no-op if db is nil, LockName is empty, neither Tables nor TablesFn
// is provided, or Enabled is false after sanitization.
func Start(ctx context.Context, db *gorm.DB, cfg Config) {
	if db == nil || cfg.LockName == "" || (len(cfg.Tables) == 0 && cfg.TablesFn == nil) {
		return
	}
	cfg = cfg.sanitized()
	if !cfg.Enabled {
		return
	}
	onceAny, _ := startOnce.LoadOrStore(cfg.LockName, &sync.Once{})
	once := onceAny.(*sync.Once)
	once.Do(func() {
		go run(ctx, db, cfg)
	})
}

func run(ctx context.Context, db *gorm.DB, cfg Config) {
	logger := slog.With("component", "tombstone_purge", "lock", cfg.LockName)
	logger.Info("tombstone purger started",
		"tables", cfg.Tables, "retention", cfg.Retention.String(),
		"interval", cfg.Interval.String(), "batch", cfg.BatchSize,
		"max_per_pass", cfg.MaxPerPass, "dry_run", cfg.DryRun)
	// Run one pass immediately (best-effort at boot), then on a timer that is
	// reset AFTER each pass completes — a time.Ticker would keep firing on a fixed
	// cadence regardless of pass duration, so a slow pass (passTimeout > Interval)
	// would make the next tick fire immediately and drive continuous, back-to-back
	// passes (DB load, replica lag). The reset guarantees a full cfg.Interval
	// BETWEEN passes.
	runPassSafe(ctx, db, cfg, logger)
	timer := time.NewTimer(cfg.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("tombstone purger stopped")
			return
		case <-timer.C:
			runPassSafe(ctx, db, cfg, logger)
			timer.Reset(cfg.Interval)
		}
	}
}

// runPassSafe runs one purge pass and recovers from a panic so a single fault
// (a bug, a misbehaving OnPurge callback, an unexpected GORM panic) can neither
// crash the host process nor permanently stop the janitor — the loop in run()
// continues to the next tick after a recovered panic. CubeDB can't import
// CubeMaster's recov package, so this is self-contained.
func runPassSafe(ctx context.Context, db *gorm.DB, cfg Config, logger *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("tombstone purge pass panicked; recovered, will retry next tick",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	runPassFn(ctx, db, cfg, logger)
}

// runPass runs one bounded purge pass for every configured table under a
// cluster-wide advisory lock. The lock is session-scoped, so acquire and
// release share one pinned connection; on any release-path error the
// connection is discarded so a held lock can never leak back into the pool.
func runPass(ctx context.Context, db *gorm.DB, cfg Config, logger *slog.Logger) {
	if err := ctx.Err(); err != nil {
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()

	acquired := false
	err := db.WithContext(passCtx).Connection(func(sess *gorm.DB) (retErr error) {
		locked, lerr := trySessionLock(sess, cfg.LockName)
		if lerr != nil {
			// Uncertain lock state: discard so the physical session (and any
			// held advisory lock) cannot re-enter the pool.
			return errors.Join(lerr, discardPinnedSession(sess))
		}
		if !locked {
			return nil // another replica owns this tick
		}
		acquired = true
		defer func() {
			// Faithful copy of artifact_gc.go's release contract: surface EVERY
			// release-path failure (releaseSessionLock error, a !released ownership
			// violation, and discardPinnedSession's own error) into the pass error
			// via errors.Join, so an uncertain lock state is never reported as a
			// successful pass.
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(passCtx), lockReleaseTimeout)
			defer relCancel()
			relSess := pinnedSessionWithContext(sess, relCtx)
			released, rerr := releaseSessionLock(relSess, cfg.LockName)
			if rerr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("release lock: %w", rerr), discardPinnedSession(sess))
				return
			}
			if !released {
				retErr = errors.Join(retErr, errors.New("release lock: current session did not hold lock"))
			}
		}()

		cutoff := time.Now().Add(-cfg.Retention)
		tables := cfg.Tables
		if cfg.TablesFn != nil {
			tables = cfg.TablesFn()
		}
		var firstErr error
		for _, table := range tables {
			if err := passCtx.Err(); err != nil {
				// Pass cut short by timeout/cancellation — surface it so the pass
				// is logged as incomplete rather than silently healthy.
				logger.Warn("tombstone purge pass cut short", "table", table, "reason", err)
				return err
			}
			purged, perr := purgeTable(passCtx, sess, table, cutoff, cfg)
			recordPurge(cfg, logger, table, purged, perr)
			if perr != nil && firstErr == nil {
				firstErr = perr
			}
		}
		return firstErr
	})
	switch {
	case err != nil:
		logger.Warn("tombstone purge pass failed", "error", err)
	case !acquired:
		logger.Info("tombstone purge lock held by another replica; skipping pass")
	}
}

// purgeTable hard-deletes (non-dry-run) or counts (DryRun) tombstoned rows in
// table older than cutoff.
//
// DryRun reports the FULL eligible backlog via a single COUNT — independent of
// MaxPerPass, which only bounds real deletes — so operators can size a backlog
// accurately (a capped sample would silently under-report a large backlog). It
// issues no DELETE.
//
// The delete path is two-step (select IDs → delete by id IN (...)) to stay
// dialect-agnostic. Under the advisory lock only one replica purges, so
// deleted==selected; the budget is still decremented by the selected count so
// a concurrent slip can never cause an unbounded loop.
func purgeTable(ctx context.Context, sess *gorm.DB, table string, cutoff time.Time, cfg Config) (int, error) {
	if cfg.DryRun {
		// Report the FULL eligible backlog (one COUNT), not a MaxPerPass-capped
		// sample: dry-run exists to size the backlog, so capping it would silently
		// under-report any backlog larger than MaxPerPass.
		var n int64
		if err := sess.WithContext(ctx).Table(table).
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Count(&n).Error; err != nil {
			return 0, fmt.Errorf("count %s: %w", table, err)
		}
		return int(n), nil
	}
	purged := 0
	budget := cfg.MaxPerPass
	for budget > 0 {
		if err := ctx.Err(); err != nil {
			return purged, err
		}
		want := min(max(1, cfg.BatchSize), budget) // >=1: avoid Limit(0) (no-limit) / negative
		var ids []uint64
		if err := sess.WithContext(ctx).Table(table).Select("id").
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Limit(want).Find(&ids).Error; err != nil {
			return purged, fmt.Errorf("select %s: %w", table, err)
		}
		if len(ids) == 0 {
			return purged, nil // drained
		}
		// Delete via GORM (not raw SQL) so the active dialector quotes the
		// identifier correctly — MySQL backticks are a syntax error on
		// PostgreSQL (which uses double quotes). Re-check the tombstone
		// predicate in the DELETE itself: the advisory lock only excludes
		// other purger replicas, not the application. CubeOps UPSERTs set
		// deleted_at=NULL on conflict (resurrect), so a row selected as a
		// tombstone can be live again by the time we delete — without this
		// re-check we would hard-delete a live row.
		res := sess.WithContext(ctx).Table(table).
			Where("id IN ? AND deleted_at IS NOT NULL AND deleted_at < ?", ids, cutoff).
			Delete(nil)
		if res.Error != nil {
			return purged, fmt.Errorf("delete %s: %w", table, res.Error)
		}
		purged += int(res.RowsAffected)
		budget -= len(ids)
		if len(ids) < want {
			return purged, nil // drained (last partial batch)
		}
	}
	return purged, nil
}

func recordPurge(cfg Config, logger *slog.Logger, table string, purged int, err error) {
	if cfg.OnPurge != nil {
		cfg.OnPurge(table, purged, err)
	}
	if err != nil {
		logger.Warn("purge table failed; continuing with next table",
			"table", table, "error", err)
		return
	}
	if purged > 0 || cfg.DryRun {
		logger.Info("tombstone purge",
			"table", table, "purged", purged, "dry_run", cfg.DryRun)
	}
}

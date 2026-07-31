//go:build integration

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"


	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// openLiveMySQL connects to a real MySQL the way the integration suite does:
// set CUBE_TEST_DB_DSN, e.g.
//
//	cube:cube_pass@tcp(127.0.0.1:3306)/cube_mvp?parseTime=true
func openLiveMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("CUBE_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("set CUBE_TEST_DB_DSN to run this integration test")
	}
	sqlDB, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("MySQL unreachable: %v", err)
	}
	return gormDB
}

// TestPreheatAdvisoryLock_PinnedConnectionReleasesCleanly is the end-to-end
// validation of the critical fix: the preheat reconcile's lock pattern — pin one
// connection via db.Connection(func(sess){...}) and run trySessionLock +
// releaseSessionLock on that pinned session — releases the advisory lock so a
// later pass (on a different connection) can re-acquire it. Runs against the
// live deployment MySQL. Under the original bug (acquire/release as two pooled
// statements) the lock leaked and the final assertion would fail.
func TestPreheatAdvisoryLock_PinnedConnectionReleasesCleanly(t *testing.T) {
	db := openLiveMySQL(t)
	ctx := context.Background()

	// Run the EXACT pattern runPreheatReconcilePass uses.
	err := db.WithContext(ctx).Connection(func(sess *gorm.DB) error {
		locked, err := trySessionLock(sess, preheatLockName)
		require.NoError(t, err)
		require.True(t, locked, "should acquire the preheat lock")

		releaseSess := pinnedSessionWithContext(sess, ctx)
		released, err := releaseSessionLock(releaseSess, preheatLockName)
		require.NoError(t, err)
		assert.True(t, released, "lock must be released on the pinned session")
		return nil
	})
	require.NoError(t, err)

	// A FRESH connection must be able to acquire the lock — proving no leak.
	var got sql.NullInt64
	require.NoError(t, db.WithContext(ctx).Raw("SELECT GET_LOCK(?, 0)", preheatLockName).Scan(&got).Error)
	assert.True(t, got.Valid && got.Int64 == 1,
		"lock leaked: a fresh connection could not acquire it after the pinned release")
	// Release the lock we just took for the assertion.
	_ = db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", preheatLockName).Error
}

// TestPreheatAdvisoryLock_TwoStatementPatternLeaks is the deterministic
// regression proof: the ORIGINAL pattern (acquire on one connection, release on
// another) silently leaks the lock. Acquire and release are forced onto two
// distinct physical connections via explicit sql.Conn checkouts.
//
// This documents exactly why runPreheatReconcilePass must pin the connection,
// and will keep anyone from "simplifying" it back to two pooled statements.
func TestPreheatAdvisoryLock_TwoStatementPatternLeaks(t *testing.T) {
	db := openLiveMySQL(t)
	ctx := context.Background()
	name := preheatLockName + "_regression"
	t.Cleanup(func() {
		// Best-effort cleanup in case a failure leaves the lock held.
		_ = db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", name).Error
	})

	pool, err := db.DB()
	require.NoError(t, err)

	// Acquire on connection A.
	connA, err := pool.Conn(ctx)
	require.NoError(t, err)
	defer connA.Close()
	var res sql.NullInt64
	require.NoError(t, connA.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&res))
	require.True(t, res.Valid && res.Int64 == 1, "must acquire on connection A")

	// "Release" on a DIFFERENT connection B (the old bug).
	connB, err := pool.Conn(ctx)
	require.NoError(t, err)
	defer connB.Close()
	var rel sql.NullInt64
	require.NoError(t, connB.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&rel))
	// MySQL RELEASE_LOCK on a non-owning session returns 0 (the lock exists but
	// is not held by this thread) — a valid, non-error result. GORM's .Error is
	// nil for it, which is exactly why the old defer's "release lock failed" log
	// never fired: the lock simply stays held. (NULL is returned only when the
	// named lock does not exist at all.)
	assert.True(t, rel.Valid && rel.Int64 == 0,
		"RELEASE_LOCK on a non-owning connection must return 0 (silent no-op that leaked the lock), got valid=%v int64=%d", rel.Valid, rel.Int64)

	// A third connection cannot acquire — the lock is still held by connection A.
	connC, err := pool.Conn(ctx)
	require.NoError(t, err)
	defer connC.Close()
	var res2 sql.NullInt64
	require.NoError(t, connC.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&res2))
	assert.False(t, res2.Valid && res2.Int64 == 1,
		"lock leaked: a third connection must NOT acquire while connection A still holds it")

	// Properly release via the owning connection so the test is repeatable.
	var final sql.NullInt64
	require.NoError(t, connA.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&final))
	assert.True(t, final.Valid && final.Int64 == 1, "owning connection must release")
}

// TestPreheatAdvisoryLock_ContentionSingleWinner simulates N CubeMaster
// replicas all firing a reconcile pass at the same instant (the multi-master
// contention case). Exactly one must win the GET_LOCK(name, 0); the rest must
// get locked=false and skip (the loser path in runPreheatReconcilePass). After
// the winner releases, a fresh connection must be able to acquire (no leak).
func TestPreheatAdvisoryLock_ContentionSingleWinner(t *testing.T) {
	db := openLiveMySQL(t)
	ctx := context.Background()
	name := preheatLockName + "_contention"
	t.Cleanup(func() { _ = db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", name).Error })

	const contenders = 6
	var wins atomic.Int32
	var attempted atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(contenders)

	for range contenders {
		go func() {
			defer done.Done()
			err := db.WithContext(ctx).Connection(func(sess *gorm.DB) error {
				<-start // release all contenders simultaneously
				locked, err := trySessionLock(sess, name)
				attempted.Add(1)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return err
				}
				if locked {
					wins.Add(1)
					// hold the lock until the main goroutine has counted every
					// contender's attempt, so a late loser can't sneak in after
					// an early winner releases.
					<-release
					_, _ = releaseSessionLock(pinnedSessionWithContext(sess, ctx), name)
				}
				return nil
			})
			if err != nil {
				t.Errorf("connection: %v", err)
			}
		}()
	}
	close(start)            // fire all GET_LOCK(name,0) at once
	for attempted.Load() < contenders {
		time.Sleep(time.Millisecond)
	}
	close(release)          // let the winner release
	done.Wait()

	assert.Equal(t, int32(1), wins.Load(),
		"exactly one master must win the advisory lock under simultaneous contention")

	// After release, a fresh connection must acquire (no leak).
	var got sql.NullInt64
	require.NoError(t, db.WithContext(ctx).Raw("SELECT GET_LOCK(?, 0)", name).Scan(&got).Error)
	assert.True(t, got.Valid && got.Int64 == 1, "lock leaked after the winner released")
	_ = db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", name).Error
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package lock provides cross-instance distributed locks backed by the
// database session layer (MySQL GET_LOCK / PostgreSQL pg_advisory_lock).
//
// These are copied (not imported) from CubeMaster/pkg/templatecenter/artifact_gc.go
// so the standalone CubeTemplateCenter process can take the same locks
// without depending on CubeMaster's internal unexported helpers. PR 5 of the
// staged split (docs/dev/templatecenter-design.md §3.3) will move the
// templatecenter package itself; at that point these helpers can be
// consolidated into CubeDB/dao/lock/.
//
// Why DB session locks and not Redis:
//   - Connection drop auto-releases (no redisson watchdog / fencing token)
//   - CubeMaster already uses them for GC / schema migration (consistency)
//   - Redis is a cache layer; making it a consistency dependency hurts HA
//
// TODO(templatecenter): no unit tests yet. Needs coverage (both mysql and
// postgres dialect branches) for: acquire/release round-trip, concurrent
// contention (second session must fail to acquire), and PinConn/
// DiscardPinnedSession actually sharing/discarding one physical connection.
package lock

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// maxMySQLLockNameLen is MySQL's hard limit for GET_LOCK/RELEASE_LOCK
// identifiers. Exceeding it fails the query with:
//
//	Error 4163 (42000): User-level lock name '...' should not exceed 64 characters.
//
// PostgreSQL's pg_advisory_lock/pg_try_advisory_lock take hashtext(name),
// which accepts arbitrary-length input, so this limit is MySQL-specific.
// normalizeLockName is still applied unconditionally (both dialects) so a
// given lock name maps to one canonical string regardless of which
// database driver is in play.
const maxMySQLLockNameLen = 64

// normalizeLockName deterministically shortens name to fit MySQL's 64-char
// GET_LOCK limit. Callers such as WithBuildLock build lock names as
// "tc_build_" + fingerprint, where fingerprint is a 64-char sha256 hex
// digest -- 73 bytes total, which blows past the limit and fails every
// build with Error 4163.
//
// The result is a pure function of the full original name, so:
//   - acquire and release always agree (both call this before touching the
//     DB, so they normalize the same input to the same output)
//   - every TC replica computes the identical string for the identical
//     input (no per-process/per-machine state involved)
//   - two different long names practically never collapse onto the same
//     short name: the suffix is a 64-bit slice of a sha256 digest of the
//     FULL name, not just the truncated prefix
//
// Short names (the common case: fixed constants like "tc_reconcile_v1")
// pass through unchanged.
func normalizeLockName(name string) string {
	if len(name) <= maxMySQLLockNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:16] // 17 bytes, fixed width
	prefixBudget := maxMySQLLockNameLen - len(suffix)
	if prefixBudget < 0 {
		prefixBudget = 0
	}
	prefix := name
	if len(prefix) > prefixBudget {
		prefix = prefix[:prefixBudget]
	}
	return prefix + suffix
}

// TrySessionLock attempts to acquire a cross-instance session lock with 0
// timeout (immediate return).
//
//   - MySQL:      SELECT GET_LOCK(name, 0)
//   - PostgreSQL: SELECT pg_try_advisory_lock(hashtext(name))
//
// name is normalized via normalizeLockName before use, so callers may pass
// names longer than MySQL's 64-char limit (e.g. a lock scoped by a sha256
// fingerprint) without pre-shortening them.
//
// The caller MUST pass a *gorm.DB pinned to one connection so that
// acquire and release share the same session. Use PinConn to obtain one.
//
// Returns:
//   - (true, nil):      lock acquired
//   - (false, nil):     lock held by another session
//   - (false, err):     lock state is uncertain; caller should DiscardPinnedSession
func TrySessionLock(sess *gorm.DB, name string) (bool, error) {
	name = normalizeLockName(name)
	dialect := sess.Dialector.Name()
	switch dialect {
	case "postgres":
		var ok bool
		if err := sess.Raw("SELECT pg_try_advisory_lock(hashtext(?))", name).Scan(&ok).Error; err != nil {
			return false, err
		}
		return ok, nil
	case "mysql":
		var res sql.NullInt64
		if err := sess.Raw("SELECT GET_LOCK(?, 0)", name).Scan(&res).Error; err != nil {
			return false, err
		}
		if !res.Valid {
			return false, fmt.Errorf("GET_LOCK %q returned NULL", name)
		}
		switch res.Int64 {
		case 1:
			return true, nil
		case 0:
			return false, nil
		default:
			return false, fmt.Errorf("GET_LOCK %q returned unexpected value %d", name, res.Int64)
		}
	default:
		return false, fmt.Errorf("unsupported database dialect %q", dialect)
	}
}

// ReleaseSessionLock releases a cross-instance session lock on the same
// connection that acquired it.
//
//   - MySQL:      SELECT RELEASE_LOCK(name)
//   - PostgreSQL: SELECT pg_advisory_unlock(hashtext(name))
//
// name is normalized via normalizeLockName before use, identically to
// TrySessionLock, so the same original name always resolves to the same
// physical lock on release as it did on acquire.
//
// Returns:
//   - (true, nil):  released (this session was the holder)
//   - (false, nil): this session is known not to hold the lock
//   - (false, err): lock state unknown; caller should DiscardPinnedSession
func ReleaseSessionLock(sess *gorm.DB, name string) (bool, error) {
	name = normalizeLockName(name)
	dialect := sess.Dialector.Name()
	switch dialect {
	case "postgres":
		var released bool
		if err := sess.Raw("SELECT pg_advisory_unlock(hashtext(?))", name).Scan(&released).Error; err != nil {
			return false, err
		}
		return released, nil
	case "mysql":
		var res sql.NullInt64
		if err := sess.Raw("SELECT RELEASE_LOCK(?)", name).Scan(&res).Error; err != nil {
			return false, err
		}
		if !res.Valid {
			return false, nil
		}
		switch res.Int64 {
		case 1:
			return true, nil
		case 0:
			return false, nil
		default:
			return false, fmt.Errorf("RELEASE_LOCK %q returned unexpected value %d", name, res.Int64)
		}
	default:
		return false, fmt.Errorf("unsupported database dialect %q", dialect)
	}
}

// PinConn returns a *gorm.DB pinned to a single *sql.Conn so that
// TrySessionLock / ReleaseSessionLock share the same session. The caller
// MUST close the returned sql.Conn (or use DiscardPinnedSession on error)
// to return it to the pool.
//
// Typical usage:
//
//	conn, sess, err := lock.PinConn(db)
//	if err != nil { return err }
//	defer conn.Close()
//	ok, err := lock.TrySessionLock(sess, name)
//	...
func PinConn(db *gorm.DB) (*sql.Conn, *gorm.DB, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("pin conn: %w", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("pin conn: %w", err)
	}
	// Pin the gorm.DB to this single *sql.Conn. Two traps matter here:
	//
	//  1. GORM's Raw path reads sess.Statement.ConnPool, NOT the (Config)
	//     ConnPool field on *gorm.DB. Setting sess.ConnPool alone leaves
	//     Statement.ConnPool pointing at the original *sql.DB pool, so
	//     GET_LOCK and RELEASE_LOCK could run on two different physical
	//     connections and never actually exclude a peer replica.
	//  2. Session{NewDB: true} does NOT materialize a private Statement — the
	//     returned handle still shares Statement with its parent until the
	//     first getInstance() (triggered here via WithContext). Pinning before
	//     that would mutate the SHARED parent Statement and redirect the whole
	//     pool through this one connection.
	//
	// CubeMaster's image_job_reconciler.go pins the same way.
	sess := db.Session(&gorm.Session{NewDB: true}).WithContext(context.Background())
	sess.Statement.ConnPool = conn
	return conn, sess, nil
}

// DiscardPinnedSession prevents a connection with an uncertain advisory-lock
// state from returning to database/sql's pool. Closing the physical session
// makes MySQL/PostgreSQL release all session-scoped locks it still owns.
func DiscardPinnedSession(sess *gorm.DB) error {
	if sess == nil || sess.Statement == nil {
		return errors.New("discard pinned session: missing GORM statement")
	}
	conn, ok := sess.Statement.ConnPool.(*sql.Conn)
	if !ok {
		return fmt.Errorf("discard pinned session: unexpected connection pool %T", sess.Statement.ConnPool)
	}
	err := conn.Raw(func(_ any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard pinned session: %w", err)
	}
	return errors.New("discard pinned session: connection remained usable")
}

// PinnedSessionWithContext derives a clean GORM session on the same pinned
// connection. The candidate query may have populated sess.Error; carrying
// that error into the release session would make GORM skip the unlock SQL
// entirely.
func PinnedSessionWithContext(sess *gorm.DB, ctx context.Context) *gorm.DB {
	clean := sess.Session(&gorm.Session{NewDB: true})
	clean.Error = nil
	return clean.WithContext(ctx)
}

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
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// TrySessionLock attempts to acquire a cross-instance session lock with 0
// timeout (immediate return).
//
//   - MySQL:      SELECT GET_LOCK(name, 0)
//   - PostgreSQL: SELECT pg_try_advisory_lock(hashtext(name))
//
// The caller MUST pass a *gorm.DB pinned to one connection so that
// acquire and release share the same session. Use PinConn to obtain one.
//
// Returns:
//   - (true, nil):      lock acquired
//   - (false, nil):     lock held by another session
//   - (false, err):     lock state is uncertain; caller should DiscardPinnedSession
func TrySessionLock(sess *gorm.DB, name string) (bool, error) {
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
// Returns:
//   - (true, nil):  released (this session was the holder)
//   - (false, nil): this session is known not to hold the lock
//   - (false, err): lock state unknown; caller should DiscardPinnedSession
func ReleaseSessionLock(sess *gorm.DB, name string) (bool, error) {
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
	// Pin the gorm.DB to this single *sql.Conn by overriding ConnPool on a
	// fresh *gorm.DB derived from the caller. Session{NewDB: true} keeps the
	// Statement clean (no leftover clauses / Error) while db.ConnPool = conn
	// is the actual pinning — gorm.Session has no ConnPool field, that lives
	// on *gorm.DB itself (gorm/gorm.go:56).
	sess := db.Session(&gorm.Session{NewDB: true})
	sess.ConnPool = conn
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

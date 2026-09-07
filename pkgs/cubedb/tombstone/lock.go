// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package tombstone provides a scheduled, bounded hard-purge of soft-deleted
// ("tombstoned") database rows. It is a DB-level janitor shared by CubeMaster
// and CubeOps: each caller supplies its own table list, retention and lock name
// (see the design doc at docs/guide/soft-delete-purge.md).
//
// The advisory-lock helpers below are a faithful copy of the battle-tested
// invariants in CubeMaster/pkg/templatecenter/artifact_gc.go. A future ticket
// may consolidate the two; they are intentionally not refactored here to avoid
// regression risk on a load-bearing lock helper.
package tombstone

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// trySessionLock attempts to acquire a cross-instance session lock with 0
// timeout (immediate return). MySQL: GET_LOCK(name, 0);
// PG: pg_try_advisory_lock(hashtext(name)). The caller MUST pass a *gorm.DB
// pinned to one connection (via db.Connection(...)) so acquire and release
// share the same physical session.
func trySessionLock(sess *gorm.DB, name string) (bool, error) {
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

// releaseSessionLock releases a cross-instance session lock on the same
// connection that acquired it. A false result means the current session is
// known not to hold the lock; an error means the lock state is unknown.
func releaseSessionLock(sess *gorm.DB, name string) (bool, error) {
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

// discardPinnedSession prevents a connection with an uncertain advisory-lock
// state from returning to database/sql's pool. Closing the physical session
// makes MySQL/PostgreSQL release all session-scoped locks it still owns.
func discardPinnedSession(sess *gorm.DB) error {
	if sess == nil || sess.Statement == nil {
		return errors.New("discard pinned session: missing GORM statement")
	}
	conn, ok := sess.Statement.ConnPool.(*sql.Conn)
	if !ok {
		return fmt.Errorf("discard pinned session: unexpected connection pool %T", sess.Statement.ConnPool)
	}
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard pinned session: %w", err)
	}
	return errors.New("discard pinned session: connection remained usable")
}

// pinnedSessionWithContext derives a clean GORM session on the same pinned
// connection. A prior query may have populated sess.Error; carrying that error
// into the release session would make GORM skip the unlock SQL entirely.
func pinnedSessionWithContext(sess *gorm.DB, ctx context.Context) *gorm.DB {
	clean := sess.Session(&gorm.Session{NewDB: true})
	clean.Error = nil
	return clean.WithContext(ctx)
}

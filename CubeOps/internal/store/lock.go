// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"

	"gorm.io/gorm"
)

// WithAdvisoryLock runs fn while holding a cluster-wide session lock. If the
// lock is already held, fn is skipped (nil error). Acquire and release share
// one pinned connection.
func (s *Store) WithAdvisoryLock(ctx context.Context, name string, fn func(context.Context) error) error {
	if s == nil || s.db == nil || name == "" {
		return fn(ctx)
	}
	return s.db.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		ok, err := trySessionLock(tx, name)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		defer func() { _, _ = releaseSessionLock(tx, name) }()
		return fn(ctx)
	})
}

func trySessionLock(sess *gorm.DB, name string) (bool, error) {
	switch sess.Dialector.Name() {
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
		return true, nil
	}
}

func releaseSessionLock(sess *gorm.DB, name string) (bool, error) {
	switch sess.Dialector.Name() {
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
		return res.Int64 == 1, nil
	default:
		return true, nil
	}
}

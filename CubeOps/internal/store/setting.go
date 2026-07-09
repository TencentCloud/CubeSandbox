// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
)

const settingMasterKey = "secret_master_key"

// GetSetting retrieves a setting value by key.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.WithContext(ctx).Raw(
		"SELECT setting_value FROM t_agenthub_setting WHERE setting_key = ? LIMIT 1", key,
	).Scan(&val).Error
	if errors.Is(err, sql.ErrNoRows) || val == "" {
		return "", nil
	}
	return val, err
}

// GetOrCreateSetting atomically gets an existing setting or creates it with the given value.
// Uses INSERT IGNORE semantics for concurrency safety.
func (s *Store) GetOrCreateSetting(ctx context.Context, key, value string) (string, error) {
	// Try INSERT IGNORE first (concurrent-safe).
	if err := s.db.WithContext(ctx).Exec(
		"INSERT IGNORE INTO t_agenthub_setting (setting_key, setting_value) VALUES (?, ?)",
		key, value,
	).Error; err != nil {
		return "", err
	}
	// Then read the winning value.
	return s.GetSetting(ctx, key)
}

// SetSetting upserts a setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.db.WithContext(ctx).Exec(
		"INSERT INTO t_agenthub_setting (setting_key, setting_value) VALUES (?, ?) "+
			"ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value)",
		key, value,
	).Error
}

// GetUserPassword retrieves the stored password hash for a user.
func (s *Store) GetUserPassword(ctx context.Context, username string) (string, error) {
	var pwd string
	err := s.db.WithContext(ctx).Raw(
		"SELECT password FROM t_agenthub_user WHERE username = ? LIMIT 1", username,
	).Scan(&pwd).Error
	if errors.Is(err, sql.ErrNoRows) || pwd == "" {
		return "", nil
	}
	return pwd, err
}

// SetUserPassword updates the password hash for a user.
func (s *Store) SetUserPassword(ctx context.Context, username, passwordHash string) error {
	result := s.db.WithContext(ctx).Exec(
		"UPDATE t_agenthub_user SET password = ? WHERE username = ?",
		passwordHash, username,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("user not found")
	}
	return nil
}

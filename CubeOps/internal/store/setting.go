// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
)

const settingMasterKey = "secret_master_key"

// ── System-level settings (t_system_setting) ────────────────────────────────

// GetSystemSetting retrieves a system-level setting value by key.
func (s *Store) GetSystemSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.WithContext(ctx).Raw(
		"SELECT setting_value FROM t_system_setting WHERE setting_key = ? LIMIT 1", key,
	).Scan(&val).Error
	if errors.Is(err, sql.ErrNoRows) || val == "" {
		return "", nil
	}
	return val, err
}

// GetOrCreateSystemSetting atomically gets an existing system setting or
// creates it with the given value. Uses INSERT IGNORE / ON CONFLICT DO
// NOTHING for concurrency safety .
func (s *Store) GetOrCreateSystemSetting(ctx context.Context, key, value string) (string, error) {
	if err := s.db.WithContext(ctx).Exec(
		insertIgnorePrefix()+" INTO t_system_setting (setting_key, setting_value) VALUES (?, ?)"+onConflictDoNothing(),
		key, value,
	).Error; err != nil {
		return "", err
	}
	return s.GetSystemSetting(ctx, key)
}

// SetSystemSetting upserts a system-level setting value.
func (s *Store) SetSystemSetting(ctx context.Context, key, value string) error {
	return s.db.WithContext(ctx).Exec(
		upsertSettingSQL("t_system_setting"),
		key, value,
	).Error
}

// ── AgentHub-level settings (t_agenthub_setting) ────────────────────────────

// GetSetting retrieves an AgentHub-level setting value by key.
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
// Uses INSERT IGNORE / ON CONFLICT DO NOTHING semantics .
func (s *Store) GetOrCreateSetting(ctx context.Context, key, value string) (string, error) {
	// Try INSERT IGNORE first (concurrent-safe).
	if err := s.db.WithContext(ctx).Exec(
		insertIgnorePrefix()+" INTO t_agenthub_setting (setting_key, setting_value) VALUES (?, ?)"+onConflictDoNothing(),
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
		upsertSettingSQL("t_agenthub_setting"),
		key, value,
	).Error
}

// ── System users (t_system_user) ────────────────────────────────────────────

// GetUserPassword retrieves the stored password hash for a user.
func (s *Store) GetUserPassword(ctx context.Context, username string) (string, error) {
	var pwd string
	err := s.db.WithContext(ctx).Raw(
		"SELECT password FROM t_system_user WHERE username = ? LIMIT 1", username,
	).Scan(&pwd).Error
	if errors.Is(err, sql.ErrNoRows) || pwd == "" {
		return "", nil
	}
	return pwd, err
}

// SetUserPassword updates the password hash for a user.
func (s *Store) SetUserPassword(ctx context.Context, username, passwordHash string) error {
	result := s.db.WithContext(ctx).Exec(
		"UPDATE t_system_user SET password = ? WHERE username = ?",
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

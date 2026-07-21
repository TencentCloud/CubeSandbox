// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	"github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/mysql"
	"github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/postgres"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"gorm.io/gorm"
)

// Store wraps the database connection and provides access to all data operations.
type Store struct {
	db *gorm.DB
}

// New opens the database connection, runs migrations, and bootstraps the master key.
func New(ctx context.Context, cfg dao.Config) (*Store, error) {
	// Register the MySQL + PostgreSQL drivers (idempotent via init()).
	_ = mysql.DriverName
	_ = postgres.DriverName

	gormDB, err := dao.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	slog.Info("running database migrations")
	if err := dao.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("schema migration failed: %w", err)
	}

	s := &Store{db: gormDB}

	// Bootstrap the encryption master key from the settings table.
	if err := s.bootstrapMasterKey(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap master key: %w", err)
	}

	// Seed the default admin account.
	if err := s.seedDefaultAdmin(ctx); err != nil {
		return nil, fmt.Errorf("seed default admin: %w", err)
	}

	slog.Info("database initialised")
	return s, nil
}

// DB returns the underlying *gorm.DB for direct use by business code.
func (s *Store) DB() *gorm.DB {
	return s.db
}

// Close releases the database connection.
func (s *Store) Close() error {
	return dao.Close()
}

// bootstrapMasterKey loads or creates the master encryption key.
//
// Resolution order (R15 dual-read for safe rollback):
//  1. t_system_setting.secret_master_key (new table, post-migration)
//  2. t_agenthub_setting.secret_master_key (old table, pre-migration / rollback)
//  3. Generate a new key and persist to t_system_setting.
//
// The migration that copies the key to t_system_setting intentionally does
// NOT delete it from t_agenthub_setting, so rolling back to the old CubeAPI
// binary (which reads from t_agenthub_setting) still finds the original key
// and can decrypt existing enc:v1 secrets.
func (s *Store) bootstrapMasterKey(ctx context.Context) error {
	// 1. Try the new system settings table.
	b64, err := s.GetSystemSetting(ctx, "secret_master_key")
	if err != nil {
		return fmt.Errorf("read master key from t_system_setting: %w", err)
	}

	if b64 == "" {
		// 2. Fallback: read from the old agenthub settings table.
		//    This covers the upgrade window where the migration has run
		//    (key copied to t_system_setting) but also the case where
		//    CubeOps starts against a DB that hasn't been migrated yet.
		b64, err = s.GetSetting(ctx, "secret_master_key")
		if err != nil {
			return fmt.Errorf("read master key from t_agenthub_setting: %w", err)
		}
	}

	if b64 != "" {
		if err := crypto.InstallMasterKey(b64); err != nil {
			return fmt.Errorf("install master key: %w", err)
		}
		slog.Info("master encryption key loaded from database")
		return nil
	}

	// 3. No existing key anywhere — generate and persist to t_system_setting.
	generated, err := crypto.GenerateMasterKeyB64()
	if err != nil {
		return fmt.Errorf("generate master key: %w", err)
	}
	b64, err = s.GetOrCreateSystemSetting(ctx, "secret_master_key", generated)
	if err != nil {
		return err
	}
	if err := crypto.InstallMasterKey(b64); err != nil {
		return fmt.Errorf("install master key: %w", err)
	}
	slog.Info("master encryption key generated and persisted to t_system_setting")
	return nil
}

// BootstrapJWTSecret loads or creates the JWT signing secret from t_system_setting.
// If JWT_SECRET env var is set, it takes priority and is NOT overwritten.
// Otherwise, a random 32-byte secret is generated, persisted to the system
// settings table, and returned.  This allows zero-config deployment — the
// secret is auto-generated on first run and reused on subsequent runs.
func (s *Store) BootstrapJWTSecret(ctx context.Context, envSecret string) (string, error) {
	if envSecret != "" {
		return envSecret, nil
	}
	// Use GetOrCreateSystemSetting (INSERT IGNORE + read-back) so that
	// concurrent starts (process restart overlap, multi-replica) all
	// converge on the same value instead of overwriting each other.
	generated, err := crypto.GenerateMasterKeyB64()
	if err != nil {
		return "", fmt.Errorf("generate JWT secret: %w", err)
	}
	winner, err := s.GetOrCreateSystemSetting(ctx, "jwt_secret", generated)
	if err != nil {
		return "", fmt.Errorf("persist JWT secret: %w", err)
	}
	if winner == generated {
		slog.Info("JWT secret auto-generated and persisted to database (t_system_setting)")
	} else {
		slog.Info("JWT secret loaded from database (t_system_setting)")
	}
	return winner, nil
}

// seedDefaultAdmin creates the default admin/admin account in t_system_user.
func (s *Store) seedDefaultAdmin(ctx context.Context) error {
	hash, err := crypto.HashPassword("admin")
	if err != nil {
		return fmt.Errorf("hash default password: %w", err)
	}
	result := s.db.WithContext(ctx).Exec(
		insertIgnorePrefix()+" INTO t_system_user (username, password) VALUES (?, ?)"+onConflictDoNothing(),
		"admin", hash,
	)
	if result.Error != nil {
		return fmt.Errorf("seed admin: %w", result.Error)
	}
	return nil
}

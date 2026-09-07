// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

type mysqlFingerprintStore struct{}

func (s *mysqlFingerprintStore) EnsureTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+fingerprintTable+` (
  version bigint NOT NULL,
  sha256 char(64) NOT NULL,
  source varchar(255) NOT NULL DEFAULT '',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return fmt.Errorf("ensure %s: %w", fingerprintTable, err)
	}
	return nil
}

func (s *mysqlFingerprintStore) LoadStored(ctx context.Context, db *sql.DB) (map[int64]storedFingerprint, error) {
	return loadStoredFingerprints(ctx, db)
}

func (s *mysqlFingerprintStore) CurrentlyApplied(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	return currentlyAppliedVersions(ctx, db, tableExistsMySQL)
}

func (s *mysqlFingerprintStore) RecordOne(ctx context.Context, db *sql.DB, fp fileFingerprint) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO `+fingerprintTable+` (version, sha256, source)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE sha256 = VALUES(sha256), source = VALUES(source)`,
		fp.version, fp.sum, fp.source,
	)
	if err != nil {
		return fmt.Errorf("record fingerprint for version %d: %w", fp.version, err)
	}
	return nil
}

func tableExistsMySQL(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check table %q exists: %w", name, err)
	}
	return n > 0, nil
}

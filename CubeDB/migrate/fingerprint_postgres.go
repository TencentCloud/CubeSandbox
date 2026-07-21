// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

type postgresFingerprintStore struct{}

func (s *postgresFingerprintStore) EnsureTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+fingerprintTable+` (
  version bigint NOT NULL,
  sha256 char(64) NOT NULL,
  source varchar(255) NOT NULL DEFAULT '',
  created_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (version)
)`)
	if err != nil {
		return fmt.Errorf("ensure %s: %w", fingerprintTable, err)
	}
	return nil
}

func (s *postgresFingerprintStore) LoadStored(ctx context.Context, db *sql.DB) (map[int64]storedFingerprint, error) {
	return loadStoredFingerprints(ctx, db)
}

func (s *postgresFingerprintStore) CurrentlyApplied(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	return currentlyAppliedVersions(ctx, db, tableExistsPostgres)
}

func (s *postgresFingerprintStore) RecordOne(ctx context.Context, db *sql.DB, fp fileFingerprint) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO `+fingerprintTable+` (version, sha256, source)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (version) DO UPDATE SET
		   sha256 = EXCLUDED.sha256,
		   source = EXCLUDED.source,
		   updated_at = CURRENT_TIMESTAMP`,
		fp.version, fp.sum, fp.source,
	)
	if err != nil {
		return fmt.Errorf("record fingerprint for version %d: %w", fp.version, err)
	}
	return nil
}

func tableExistsPostgres(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = current_schema() AND table_name = $1`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check table %q exists: %w", name, err)
	}
	return n > 0, nil
}

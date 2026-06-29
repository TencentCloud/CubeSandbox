// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// fingerprintStore abstracts the dialect-specific DDL and DML for the
// content-fingerprint defence layer. Each dialect provides its own
// implementation that handles table creation, data reads, and upserts
// using engine-native syntax.
type fingerprintStore interface {
	// EnsureTable idempotently creates the fingerprint bookkeeping table.
	EnsureTable(ctx context.Context, db *sql.DB) error

	// LoadStored returns all recorded fingerprints keyed by version.
	LoadStored(ctx context.Context, db *sql.DB) (map[int64]storedFingerprint, error)

	// CurrentlyApplied returns the set of versions goose has applied.
	CurrentlyApplied(ctx context.Context, db *sql.DB) (map[int64]bool, error)

	// RecordOne upserts the fingerprint for a single applied version.
	RecordOne(ctx context.Context, db *sql.DB, fp fileFingerprint) error
}

// dialectFingerprintStores maps dialect name -> store implementation.
var dialectFingerprintStores = map[string]fingerprintStore{
	"mysql":    &mysqlFingerprintStore{},
	"postgres": &postgresFingerprintStore{},
}

// resolveFingerprintStore returns the store for the given dialect.
func resolveFingerprintStore(dialect string) (fingerprintStore, error) {
	s, ok := dialectFingerprintStores[dialect]
	if !ok {
		return nil, fmt.Errorf("migrate: no fingerprint store for dialect %q", dialect)
	}
	return s, nil
}

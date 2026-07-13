// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate

import (
	"context"
	"database/sql"
)

// Dialect-specific fingerprint DDL/DML; nil dialectSpec.store disables the layer.
type fingerprintStore interface {
	EnsureTable(ctx context.Context, db *sql.DB) error
	LoadStored(ctx context.Context, db *sql.DB) (map[int64]storedFingerprint, error)
	CurrentlyApplied(ctx context.Context, db *sql.DB) (map[int64]bool, error)
	RecordOne(ctx context.Context, db *sql.DB, fp fileFingerprint) error
}

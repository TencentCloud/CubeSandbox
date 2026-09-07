// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package dao provides a thin, driver-agnostic data-access facade for
// CubeMaster. It owns the database/sql connection lifecycle, hands a GORM
// handle to business packages, and orchestrates schema migration via goose.
//
// The package is deliberately "thin": business code keeps using *gorm.DB
// directly. Swapping the underlying engine (MySQL → PostgreSQL/SQLite/…)
// requires only adding a Driver implementation and a sibling migrations
// sub-directory; no business code needs to change.
package dao

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3/lock"
	"gorm.io/gorm"
)

// Driver abstracts everything an engine-specific package must provide so the
// generic facade in dao.go can stay engine-agnostic.
type Driver interface {
	// Name returns a short, stable identifier (e.g. "mysql"). It is used as
	// the sub-directory name under migrations/ and as the goose dialect.
	Name() string

	// Open returns an open *sql.DB and a GORM handle wrapping it. The
	// returned *sql.DB MUST be the same handle that backs *gorm.DB so that
	// goose can run its migrations on the very same connection pool.
	Open(ctx context.Context, cfg Config) (*sql.DB, *gorm.DB, error)

	// SessionLocker returns a SessionLocker that wraps the entire
	// Up()/Down() run with an engine-native cluster-wide lock. It is the
	// "outer" half of the two-layer locking scheme; the "inner" per-file
	// lock is asserted from inside each migration SQL via
	// CALL cubemaster_acquire_migration_lock(...) (see migrations).
	SessionLocker(cfg Config) lock.SessionLocker
}

// Config captures the minimal data every driver needs. Concrete drivers may
// read additional, engine-specific knobs out of the Extra map. The shape is
// stable; new engines extend via Extra rather than by growing required
// fields, so swapping engines never silently invalidates an old yaml.
type Config struct {
	Driver string

	Addr   string
	User   string
	Pwd    string
	DBName string

	ConnTimeoutSeconds     int
	ReadTimeoutSeconds     int
	WriteTimeoutSeconds    int
	MaxIdleConns           int
	MaxOpenConns           int
	MaxConnLifeTimeSeconds int

	// MigrationLockTimeoutSeconds bounds GET_LOCK / pg_advisory_lock waits.
	// Defaults to 60s when zero.
	MigrationLockTimeoutSeconds int

	Extra map[string]string
}

// driverRegistry holds every linked driver. Drivers register themselves
// in their package init() so callers only need to blank-import them.
var driverRegistry = map[string]Driver{}

// Register makes a driver available by name. Re-registering the same name
// panics; this is a programmer error caught at process start, not a
// recoverable runtime condition.
func Register(d Driver) {
	if d == nil {
		panic("dao: Register(nil) driver")
	}
	name := d.Name()
	if name == "" {
		panic("dao: driver returned empty Name")
	}
	if _, exists := driverRegistry[name]; exists {
		panic(fmt.Sprintf("dao: driver %q already registered", name))
	}
	driverRegistry[name] = d
}

// resolveDriver returns the driver registered for the given name. An
// unknown name is an explicit, non-recoverable misconfiguration so the
// caller should fail fast.
func resolveDriver(name string) (Driver, error) {
	if name == "" {
		return nil, fmt.Errorf("dao: driver name is empty")
	}
	d, ok := driverRegistry[name]
	if !ok {
		return nil, fmt.Errorf("dao: driver %q is not registered (did you forget a blank import?)", name)
	}
	return d, nil
}

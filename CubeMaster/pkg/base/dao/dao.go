// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package dao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
	"gorm.io/gorm"
)

// ErrNotOpened is returned by Default() before any successful Open() call.
// It exists so callers can fail gracefully in init-order bugs instead of
// nil-deref'ing a *gorm.DB.
var ErrNotOpened = errors.New("dao: Open has not been called yet")

// handle bundles the raw *sql.DB and the GORM wrapper sharing the same
// connection pool. We keep both because:
//   - GORM is the ergonomic façade business code uses;
//   - goose needs the raw *sql.DB to run migrations on the same pool.
type handle struct {
	sqlDB    *sql.DB
	gormDB   *gorm.DB
	driver   Driver
	identity configIdentity
	locker   lock.SessionLocker
}

type configIdentity struct {
	Driver                      string
	Addr                        string
	User                        string
	Pwd                         string
	DBName                      string
	ConnTimeoutSeconds          int
	ReadTimeoutSeconds          int
	WriteTimeoutSeconds         int
	MaxIdleConns                int
	MaxOpenConns                int
	MaxConnLifeTimeSeconds      int
	MigrationLockTimeoutSeconds int
}

func (i configIdentity) String() string {
	return fmt.Sprintf("driver=%s addr=%s user=%s db=%s conn_timeout=%d read_timeout=%d write_timeout=%d max_idle=%d max_open=%d max_lifetime=%d migration_lock_timeout=%d",
		i.Driver, i.Addr, i.User, i.DBName,
		i.ConnTimeoutSeconds, i.ReadTimeoutSeconds, i.WriteTimeoutSeconds,
		i.MaxIdleConns, i.MaxOpenConns, i.MaxConnLifeTimeSeconds,
		i.MigrationLockTimeoutSeconds)
}

var (
	globalMu sync.RWMutex
	global   *handle
)

// Open establishes the shared database connection. It is safe to call
// multiple times; subsequent calls with the same Driver+DSN are no-ops
// (idempotent), while a different DSN/driver returns an error rather than
// silently clobbering the global handle.
func Open(ctx context.Context, cfg Config) (*gorm.DB, error) {
	cfg = normalizeConfig(cfg)
	drv, err := resolveDriver(cfg.Driver)
	if err != nil {
		return nil, err
	}
	identity := identityFromConfig(cfg)
	globalMu.Lock()
	defer globalMu.Unlock()
	if global != nil {
		if global.identity == identity {
			return global.gormDB, nil
		}
		return nil, fmt.Errorf("dao: already opened with %s (requested %s)",
			global.identity, identity)
	}
	sqlDB, gormDB, err := drv.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dao: open %q: %w", drv.Name(), err)
	}
	global = &handle{
		sqlDB:    sqlDB,
		gormDB:   gormDB,
		driver:   drv,
		identity: identity,
		locker:   drv.SessionLocker(cfg),
	}
	return gormDB, nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.Driver == "" {
		// Backwards-compat: callers that pre-date the multi-driver split
		// pass empty Driver to mean "MySQL". The mysql driver registers
		// itself under the name "mysql".
		cfg.Driver = "mysql"
	}
	cfg.Addr = normalizeAddr(cfg.Addr)
	return cfg
}

func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "" {
		return addr
	}
	return net.JoinHostPort("localhost", port)
}

func identityFromConfig(cfg Config) configIdentity {
	return configIdentity{
		Driver:                      cfg.Driver,
		Addr:                        cfg.Addr,
		User:                        cfg.User,
		Pwd:                         cfg.Pwd,
		DBName:                      cfg.DBName,
		ConnTimeoutSeconds:          cfg.ConnTimeoutSeconds,
		ReadTimeoutSeconds:          cfg.ReadTimeoutSeconds,
		WriteTimeoutSeconds:         cfg.WriteTimeoutSeconds,
		MaxIdleConns:                cfg.MaxIdleConns,
		MaxOpenConns:                cfg.MaxOpenConns,
		MaxConnLifeTimeSeconds:      cfg.MaxConnLifeTimeSeconds,
		MigrationLockTimeoutSeconds: cfg.MigrationLockTimeoutSeconds,
	}
}

// Default returns the global GORM handle established by Open. It panics
// only when called before Open; this lets business packages keep their
// "fail loudly on misconfiguration" stance without burying nil checks.
func Default() *gorm.DB {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if global == nil {
		panic(ErrNotOpened)
	}
	return global.gormDB
}

// SQL returns the raw *sql.DB. Reserved for the migration package and
// integration tests; business code should not use it.
func SQL() *sql.DB {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if global == nil {
		panic(ErrNotOpened)
	}
	return global.sqlDB
}

// Migrate runs every pending schema migration under the configured
// driver's dialect, taking the driver-provided cluster-wide SessionLocker
// so multiple CubeMaster instances starting up simultaneously serialize
// safely. Returns nil when the database is already at HEAD.
func Migrate(ctx context.Context) error {
	globalMu.RLock()
	h := global
	globalMu.RUnlock()
	if h == nil {
		return ErrNotOpened
	}
	return migrate.Run(ctx, h.sqlDB, h.driver.Name(), h.locker)
}

// Close releases the underlying *sql.DB. Subsequent Default()/SQL() calls
// panic; callers must Open() again before use. Intended for tests.
func Close() error {
	globalMu.Lock()
	defer globalMu.Unlock()
	if global == nil {
		return nil
	}
	err := global.sqlDB.Close()
	global = nil
	return err
}

// HealthCheck pings the database with a short timeout. It is used by the
// startup sequence in main.go to fail-fast if the DB is unreachable.
func HealthCheck(ctx context.Context, timeout time.Duration) error {
	globalMu.RLock()
	h := global
	globalMu.RUnlock()
	if h == nil {
		return ErrNotOpened
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return h.sqlDB.PingContext(pingCtx)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package mysql plugs the MySQL engine into pkg/base/dao. Blank-import it
// from main.go (or the integration test bootstrap) so the driver registers
// itself with the dao registry.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql driver
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	// DriverName is the canonical short name; it doubles as the
	// migrations sub-directory under pkg/base/dao/migrate/migrations.
	DriverName = "mysql"

	// globalLockName is the GET_LOCK key held for the entire goose.Up()
	// (outer layer of the two-layer locking scheme). Keep it stable across
	// versions; renaming it would let a paused old instance and a new
	// instance both acquire the lock and race.
	globalLockName = "cubemaster_schema_migration_global"

	defaultLockTimeoutSeconds = 60
)

func init() {
	dao.Register(&driver{})
}

type driver struct{}

func (d *driver) Name() string { return DriverName }

func (d *driver) Open(ctx context.Context, cfg dao.Config) (*sql.DB, *gorm.DB, error) {
	_ = ctx
	dsn := buildDSN(cfg)
	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("sql.Open: %w", err)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxConnLifeTimeSeconds > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.MaxConnLifeTimeSeconds) * time.Second)
	}
	gormDB, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("gorm.Open: %w", err)
	}
	return sqlDB, gormDB, nil
}

func (d *driver) SessionLocker(cfg dao.Config) lock.SessionLocker {
	timeout := cfg.MigrationLockTimeoutSeconds
	if timeout <= 0 {
		timeout = defaultLockTimeoutSeconds
	}
	return &sessionLocker{
		name:    globalLockName,
		timeout: timeout,
	}
}

// buildDSN keeps DSN composition local to the driver so callers never
// hand-craft format strings (and so we control charset / parseTime / loc).
func buildDSN(cfg dao.Config) string {
	connTimeout := cfg.ConnTimeoutSeconds
	if connTimeout <= 0 {
		connTimeout = 5
	}
	readTimeout := cfg.ReadTimeoutSeconds
	if readTimeout <= 0 {
		readTimeout = 5
	}
	writeTimeout := cfg.WriteTimeoutSeconds
	if writeTimeout <= 0 {
		writeTimeout = 5
	}
	return fmt.Sprintf(
		"%s:%s@tcp(%s)/%s?charset=utf8&parseTime=true&loc=Local&timeout=%ds&readTimeout=%ds&writeTimeout=%ds",
		cfg.User, cfg.Pwd, cfg.Addr, cfg.DBName,
		connTimeout, readTimeout, writeTimeout,
	)
}

// sessionLocker implements goose.SessionLocker on top of MySQL's
// GET_LOCK / RELEASE_LOCK. GET_LOCK is session-scoped: when the *sql.Conn
// goose uses goes away (process crash, broken pipe), MySQL releases the
// lock automatically — there is no need for a janitor / TTL.
type sessionLocker struct {
	name    string
	timeout int
}

// SessionLock blocks for up to s.timeout seconds. A return value of 0
// from GET_LOCK means the wait timed out — that is a hard error because
// another instance is still mid-migration; failing fast lets the operator
// see the queue rather than silently skip migrations.
func (s *sessionLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		"SELECT GET_LOCK(?, ?)", s.name, s.timeout).Scan(&got); err != nil {
		return fmt.Errorf("acquire migration lock %q: %w", s.name, err)
	}
	if !got.Valid {
		return fmt.Errorf("acquire migration lock %q: returned NULL (connection killed?)", s.name)
	}
	if got.Int64 != 1 {
		return fmt.Errorf("acquire migration lock %q: timeout after %ds", s.name, s.timeout)
	}
	return nil
}

// SessionUnlock is best-effort: if the lock has already been released by
// connection death there is nothing to do, and surfacing such errors would
// mask the real (preceding) failure.
func (s *sessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", s.name)
	return err
}

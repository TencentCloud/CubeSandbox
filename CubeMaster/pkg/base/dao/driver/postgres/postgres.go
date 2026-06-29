// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package postgres plugs the PostgreSQL engine into pkg/base/dao.
// Blank-import it from main.go (or the integration test bootstrap) so
// the driver registers itself with the dao registry.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver: "pgx"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	// DriverName is the canonical short name; it doubles as the
	// migrations sub-directory under pkg/base/dao/migrate/migrations.
	DriverName = "postgres"

	// advisoryLockID is the pg_advisory_lock key held for the entire
	// goose.Up() run (outer layer of the two-layer locking scheme).
	// The value is arbitrary but MUST remain stable across versions;
	// changing it would let a paused old instance and a new instance
	// both acquire the lock and race. pg_advisory_lock accepts bigint,
	// so any int64 value is valid.
	advisoryLockID = 3764529487

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
	sqlDB, err := sql.Open("pgx", dsn)
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
	gormDB, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: sqlDB}), &gorm.Config{})
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
		id:      advisoryLockID,
		timeout: timeout,
	}
}

// buildDSN constructs a PostgreSQL connection string from the dao.Config.
// It uses the key=value format accepted by libpq / pgx.
//
// TLS is controlled by the "sslmode" key in cfg.Extra (e.g. "require",
// "verify-full"). Defaults to "disable" for dev/test parity with the MySQL
// driver; production deployments SHOULD set sslmode explicitly.
//
// statement_timeout (milliseconds) is derived from WriteTimeoutSeconds as a
// server-side query time limit. This is not a perfect equivalent of MySQL's
// network-level readTimeout/writeTimeout, but it prevents hung queries from
// holding a connection pool slot indefinitely. Business code should still
// pass context deadlines for fine-grained control.
func buildDSN(cfg dao.Config) string {
	connTimeout := cfg.ConnTimeoutSeconds
	if connTimeout <= 0 {
		connTimeout = 5
	}

	sslmode := "disable"
	if v, ok := cfg.Extra["sslmode"]; ok && v != "" {
		sslmode = v
	}

	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
		cfg.Addr, cfg.User, cfg.Pwd, cfg.DBName, sslmode, connTimeout,
	)

	// Append additional libpq options via the options parameter.
	// statement_timeout is in milliseconds.
	var options []string
	if cfg.WriteTimeoutSeconds > 0 {
		options = append(options, fmt.Sprintf("-c statement_timeout=%d", cfg.WriteTimeoutSeconds*1000))
	}
	if cfg.ReadTimeoutSeconds > 0 {
		options = append(options, fmt.Sprintf("-c idle_in_transaction_session_timeout=%d", cfg.ReadTimeoutSeconds*1000))
	}
	if len(options) > 0 {
		dsn += fmt.Sprintf(" options='%s'", strings.Join(options, " "))
	}

	return dsn
}

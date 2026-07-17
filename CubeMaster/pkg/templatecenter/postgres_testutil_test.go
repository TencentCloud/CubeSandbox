// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/pressly/goose/v3/lock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/migrate"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	pgTestDSNEnv          = "CUBEMASTER_DAO_TEST_POSTGRES_DSN"
	pgRequireDockerEnv    = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	pgDockerImage         = "postgres"
	pgDockerImageTag      = "16-alpine"
	pgContainerProbeLimit = 90 * time.Second
)

type pgDockerEnv struct {
	dsn      string
	teardown func()
}

func requirePGDockerTests() bool {
	v := os.Getenv(pgRequireDockerEnv)
	if v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	ci := os.Getenv("CI")
	return ci == "true" || ci == "1"
}

func skipOrFailPGDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if requirePGDockerTests() {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

func newPGDockerEnv(t *testing.T) *pgDockerEnv {
	t.Helper()
	if dsn := os.Getenv(pgTestDSNEnv); dsn != "" {
		t.Logf("using external PostgreSQL from %s", pgTestDSNEnv)
		return &pgDockerEnv{dsn: dsn, teardown: func() {}}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		skipOrFailPGDocker(t, "dockertest not available (%v); set %s", err, pgTestDSNEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		skipOrFailPGDocker(t, "docker daemon not reachable (%v); set %s", err, pgTestDSNEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: pgDockerImage,
		Tag:        pgDockerImageTag,
		Env: []string{
			"POSTGRES_USER=cube",
			"POSTGRES_PASSWORD=cube_pass",
			"POSTGRES_DB=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		skipOrFailPGDocker(t, "could not start postgres container (%v); set %s", err, pgTestDSNEnv)
	}
	port := resource.GetPort("5432/tcp")
	dsn := fmt.Sprintf(
		"host=127.0.0.1 port=%s user=cube password=cube_pass dbname=cube_test sslmode=disable",
		port,
	)
	pool.MaxWait = pgContainerProbeLimit
	if err := pool.Retry(func() error {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("postgres container never became reachable: %v", err)
	}
	return &pgDockerEnv{
		dsn: dsn,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

type pgTestLocker struct {
	id      int64
	timeout int
}

func (l *pgTestLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	deadline := time.Now().Add(time.Duration(l.timeout) * time.Second)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", l.id).Scan(&acquired); err != nil {
			return err
		}
		if acquired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pg advisory lock timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (l *pgTestLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", l.id)
	return err
}

func openMigratedPostgresGORM(t *testing.T, env *pgDockerEnv) *gorm.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", env.dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.Run(ctx, sqlDB, "postgres", &pgTestLocker{id: 424242, timeout: 30}); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("migrate.Run: %v", err)
	}
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("gorm.Open: %v", err)
	}
	t.Cleanup(func() {
		raw, cerr := gormDB.DB()
		if cerr == nil {
			_ = raw.Close()
		}
	})
	return gormDB
}

var _ lock.SessionLocker = (*pgTestLocker)(nil)

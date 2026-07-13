// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package migrate_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// Docker missing: skip locally; CI / CUBEMASTER_REQUIRE_DOCKER_TESTS=1 → Fatal.

const (
	mysqlDSNEnv           = "CUBEMASTER_DAO_TEST_MYSQL_DSN"
	postgresDSNEnv        = "CUBEMASTER_DAO_TEST_POSTGRES_DSN"
	requireDockerTestsEnv = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	mysqlImage            = "mysql"
	mysqlImageTag         = "8.0"
	postgresImage         = "postgres"
	postgresImageTag      = "16-alpine"
	containerProbeTimeout = 90 * time.Second
)

type dbTestEnv struct {
	dsn        string
	teardown   func()
	usesDocker bool
}

func requireDockerTests() bool {
	v := os.Getenv(requireDockerTestsEnv)
	if v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	ci := os.Getenv("CI")
	return ci == "true" || ci == "1"
}

func abortOrSkipDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if requireDockerTests() {
		t.Fatalf("%s (set %s / external DSN, or fix Docker — CI forbids skip)", msg, requireDockerTestsEnv)
	}
	t.Skipf("%s", msg)
}

func newMySQLEnv(t *testing.T) *dbTestEnv {
	t.Helper()
	if dsn := os.Getenv(mysqlDSNEnv); dsn != "" {
		t.Logf("using external MySQL from %s", mysqlDSNEnv)
		return &dbTestEnv{dsn: dsn, teardown: func() {}, usesDocker: false}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipDocker(t, "dockertest not available (%v); set %s to run this test", err, mysqlDSNEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipDocker(t, "docker daemon not reachable (%v); set %s to run this test", err, mysqlDSNEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: mysqlImage,
		Tag:        mysqlImageTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		abortOrSkipDocker(t, "could not start mysql container (%v); set %s", err, mysqlDSNEnv)
	}
	port := resource.GetPort("3306/tcp")
	// Mirror mysql.buildDSN; multiStatements intentionally NOT enabled.
	dsn := fmt.Sprintf(
		"root:root@tcp(127.0.0.1:%s)/cube_test?charset=utf8&parseTime=true&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s",
		port,
	)

	pool.MaxWait = containerProbeTimeout
	if err := pool.Retry(func() error {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("mysql container never became reachable: %v", err)
	}

	return &dbTestEnv{
		dsn:        dsn,
		usesDocker: true,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

func newPostgresEnv(t *testing.T) *dbTestEnv {
	t.Helper()
	if dsn := os.Getenv(postgresDSNEnv); dsn != "" {
		t.Logf("using external PostgreSQL from %s", postgresDSNEnv)
		return &dbTestEnv{dsn: dsn, teardown: func() {}, usesDocker: false}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipDocker(t, "dockertest not available (%v); set %s to run this test", err, postgresDSNEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipDocker(t, "docker daemon not reachable (%v); set %s to run this test", err, postgresDSNEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: postgresImage,
		Tag:        postgresImageTag,
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
		abortOrSkipDocker(t, "could not start postgres container (%v); set %s", err, postgresDSNEnv)
	}
	port := resource.GetPort("5432/tcp")
	dsn := fmt.Sprintf(
		"host=127.0.0.1 port=%s user=cube password=cube_pass dbname=cube_test sslmode=disable",
		port,
	)

	pool.MaxWait = containerProbeTimeout
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

	return &dbTestEnv{
		dsn:        dsn,
		usesDocker: true,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

func openMySQLDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open(mysql): %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping mysql: %v", err)
	}
	return db
}

func openPostgresDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open(pgx): %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping postgres: %v", err)
	}
	return db
}

func TestRequireDockerTests_Env(t *testing.T) {
	t.Setenv(requireDockerTestsEnv, "")
	t.Setenv("CI", "")
	if requireDockerTests() {
		t.Fatal("expected requireDockerTests=false with empty env")
	}
	t.Setenv(requireDockerTestsEnv, "1")
	if !requireDockerTests() {
		t.Fatal("expected requireDockerTests=true when CUBEMASTER_REQUIRE_DOCKER_TESTS=1")
	}
	t.Setenv(requireDockerTestsEnv, "")
	t.Setenv("CI", "true")
	if !requireDockerTests() {
		t.Fatal("expected requireDockerTests=true when CI=true")
	}
}

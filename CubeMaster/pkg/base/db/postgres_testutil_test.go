// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package db_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

const (
	postgresDSNEnv        = "CUBEMASTER_DAO_TEST_POSTGRES_DSN"
	requireDockerTestsEnv = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	postgresImage         = "postgres"
	postgresImageTag      = "16-alpine"
	containerProbeTimeout = 90 * time.Second
)

type postgresTestEnv struct {
	addr     string
	teardown func()
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
		t.Fatal(msg)
	}
	t.Skip(msg)
}

func newPostgresTestEnv(t *testing.T) *postgresTestEnv {
	t.Helper()
	if dsn := os.Getenv(postgresDSNEnv); dsn != "" {
		t.Logf("using external PostgreSQL from %s", postgresDSNEnv)
		db := openPostgresDB(t, dsn)
		defer db.Close()
		host, port, err := splitHostPortFromDSN(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", postgresDSNEnv, err)
		}
		return &postgresTestEnv{
			addr:     host + ":" + port,
			teardown: func() {},
		}
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipDocker(t, "dockertest not available (%v); set %s", err, postgresDSNEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipDocker(t, "docker daemon not reachable (%v); set %s", err, postgresDSNEnv)
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

	return &postgresTestEnv{
		addr: "127.0.0.1:" + port,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
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

func splitHostPortFromDSN(dsn string) (host, port string, err error) {
	for _, part := range strings.Fields(dsn) {
		if strings.HasPrefix(part, "host=") {
			host = strings.TrimPrefix(part, "host=")
		}
		if strings.HasPrefix(part, "port=") {
			port = strings.TrimPrefix(part, "port=")
		}
	}
	if host == "" || port == "" {
		return "", "", fmt.Errorf("missing host/port in dsn %q", dsn)
	}
	return host, port, nil
}

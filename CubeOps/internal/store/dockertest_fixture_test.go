// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// dockertest_fixture_test.go provides a shared MySQL test environment for
// CubeOps integration tests. It mirrors the pattern used by
// cubedb/migrate/dockertest_fixture_test.go: spin up a throwaway MySQL 8.0
// container, run migrations, and hand the caller a fully-initialised *store.Store.
//
// Docker missing → skip locally; CUBEOPS_REQUIRE_DOCKER_TESTS=1 or CI=true → Fatal.

const (
	mysqlImageTag         = "8.0"
	requireDockerTestsEnv = "CUBEOPS_REQUIRE_DOCKER_TESTS"
	containerProbeTimeout = 90 * time.Second
)

// testStoreEnv holds the test database connection + teardown function.
type testStoreEnv struct {
	store    *store.Store
	dsn      string
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
		t.Fatalf("%s (set %s or fix Docker — CI forbids skip)", msg, requireDockerTestsEnv)
	}
	t.Skipf("%s", msg)
}

// newTestStore spins up a MySQL container, runs migrations, bootstraps the
// master key, and returns a ready-to-use *store.Store. The caller must defer
// env.teardown().
func newTestStore(t *testing.T) *testStoreEnv {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipDocker(t, "dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipDocker(t, "docker daemon not reachable (%v)", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "mysql",
		Tag:        mysqlImageTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cubeops_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		abortOrSkipDocker(t, "could not start mysql container (%v)", err)
	}

	port := resource.GetPort("3306/tcp")
	// The DAO config uses individual fields, not a DSN string.
	cfg := dao.Config{
		Driver:       "mysql",
		User:         "root",
		Pwd:          "root",
		Addr:         fmt.Sprintf("127.0.0.1:%s", port),
		DBName:       "cubeops_test",
		MaxIdleConns: 5,
		MaxOpenConns: 10,
	}

	pool.MaxWait = containerProbeTimeout
	if err := pool.Retry(func() error {
		db, err := sql.Open("mysql", fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/cubeops_test?charset=utf8&parseTime=true", port))
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("mysql container never became reachable: %v", err)
	}

	// store.New() handles the full bootstrap:
	//   1. dao.Open → connects + runs goose migrations
	//   2. bootstrapMasterKey → generates + installs the crypto master key
	//   3. seedDefaultAdmin → creates admin/admin account
	// We reset the global key first so each test container gets its own
	// freshly-generated key (InstallMasterKey rejects re-install with a
	// different value, which would happen when tests run sequentially with
	// different Docker containers).
	crypto.ResetMasterKeyForTest()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s, err := store.New(ctx, cfg)
	if err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("store.New: %v", err)
	}

	return &testStoreEnv{
		store: s,
		dsn:   fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/cubeops_test", port),
		teardown: func() {
			_ = s.Close()
			_ = pool.Purge(resource)
		},
	}
}

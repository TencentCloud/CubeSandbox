// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package tombstone

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Docker missing: skip locally; CI / CUBEMASTER_REQUIRE_DOCKER_TESTS=1 → Fatal.
// Mirrors CubeDB/migrate/dockertest_fixture_test.go.

const (
	mysqlDSNEnv           = "CUBEMASTER_DAO_TEST_MYSQL_DSN"
	postgresDSNEnv        = "CUBEMASTER_DAO_TEST_POSTGRES_DSN"
	requireDockerTestsEnv = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	containerProbeTimeout = 90 * time.Second
	testTable             = "t_tombstone_test"
)

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

type gormEnv struct {
	db       *gorm.DB
	teardown func()
}

func newGormEnv(t *testing.T, driver string) *gormEnv {
	t.Helper()
	switch driver {
	case "mysql":
		dsn := os.Getenv(mysqlDSNEnv)
		if dsn != "" {
			db := openGorm(t, "mysql", dsn)
			return &gormEnv{db: db, teardown: func() {}}
		}
		pool, err := dockertest.NewPool("")
		if err != nil || pool.Client.Ping() != nil {
			abortOrSkipDocker(t, "docker not available; set %s", mysqlDSNEnv)
		}
		res, err := pool.RunWithOptions(&dockertest.RunOptions{
			Repository: "mysql", Tag: "8.0",
			Env: []string{"MYSQL_ROOT_PASSWORD=root", "MYSQL_DATABASE=cube_test"},
		}, func(hc *docker.HostConfig) {
			hc.AutoRemove = true
			hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
		})
		if err != nil {
			abortOrSkipDocker(t, "could not start mysql (%v); set %s", err, mysqlDSNEnv)
		}
		dsn = fmt.Sprintf("root:root@tcp(127.0.0.1:%s)/cube_test?charset=utf8&parseTime=true&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s", res.GetPort("3306/tcp"))
		pool.MaxWait = containerProbeTimeout
		if err := pool.Retry(func() error {
			d, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
			if err != nil {
				return err
			}
			sqlDB, _ := d.DB()
			defer sqlDB.Close()
			return sqlDB.Ping()
		}); err != nil {
			_ = pool.Purge(res)
			t.Fatalf("mysql never reachable: %v", err)
		}
		db := openGorm(t, "mysql", dsn)
		return &gormEnv{db: db, teardown: func() { _ = pool.Purge(res) }}
	case "postgres":
		dsn := os.Getenv(postgresDSNEnv)
		if dsn != "" {
			db := openGorm(t, "postgres", dsn)
			return &gormEnv{db: db, teardown: func() {}}
		}
		pool, err := dockertest.NewPool("")
		if err != nil || pool.Client.Ping() != nil {
			abortOrSkipDocker(t, "docker not available; set %s", postgresDSNEnv)
		}
		res, err := pool.RunWithOptions(&dockertest.RunOptions{
			Repository: "postgres", Tag: "16-alpine",
			Env: []string{"POSTGRES_USER=cube", "POSTGRES_PASSWORD=cube_pass", "POSTGRES_DB=cube_test"},
		}, func(hc *docker.HostConfig) {
			hc.AutoRemove = true
			hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
		})
		if err != nil {
			abortOrSkipDocker(t, "could not start postgres (%v); set %s", err, postgresDSNEnv)
		}
		dsn = fmt.Sprintf("host=127.0.0.1 port=%s user=cube password=cube_pass dbname=cube_test sslmode=disable", res.GetPort("5432/tcp"))
		pool.MaxWait = containerProbeTimeout
		if err := pool.Retry(func() error {
			d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
			if err != nil {
				return err
			}
			sqlDB, _ := d.DB()
			defer sqlDB.Close()
			return sqlDB.Ping()
		}); err != nil {
			_ = pool.Purge(res)
			t.Fatalf("postgres never reachable: %v", err)
		}
		db := openGorm(t, "postgres", dsn)
		return &gormEnv{db: db, teardown: func() { _ = pool.Purge(res) }}
	default:
		t.Fatalf("unknown driver %q", driver)
		return nil
	}
}

func openGorm(t *testing.T, driver, dsn string) *gorm.DB {
	t.Helper()
	var (
		db  *gorm.DB
		err error
	)
	if driver == "postgres" {
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	} else {
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	}
	if err != nil {
		t.Fatalf("gorm.Open(%s): %v", driver, err)
	}
	return db
}

func createTestTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("DROP TABLE IF EXISTS " + testTable).Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	switch db.Dialector.Name() {
	case "postgres":
		db.Exec(`CREATE TABLE ` + testTable + ` (
			id BIGSERIAL PRIMARY KEY,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at TIMESTAMP DEFAULT NULL
		)`)
	default:
		db.Exec(`CREATE TABLE ` + testTable + ` (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			deleted_at DATETIME DEFAULT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB`)
	}
	if err := db.Exec("CREATE INDEX idx_" + testTable + "_deleted_at ON " + testTable + " (deleted_at)").Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
}

// seed inserts n rows with the given deleted_at value (nil → live row).
func seed(t *testing.T, db *gorm.DB, deletedAt *time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var res *gorm.DB
		if deletedAt == nil {
			res = db.Exec("INSERT INTO " + testTable + " (deleted_at) VALUES (NULL)")
		} else {
			res = db.Exec("INSERT INTO "+testTable+" (deleted_at) VALUES (?)", *deletedAt)
		}
		if res.Error != nil {
			t.Fatalf("seed: %v", res.Error)
		}
	}
}

func countRows(t *testing.T, db *gorm.DB, where string) int64 {
	t.Helper()
	var n int64
	if err := db.Table(testTable).Where(where).Count(&n).Error; err != nil {
		t.Fatalf("count(%s): %v", where, err)
	}
	return n
}

func TestPurgeTable_DeletesOnlyOldTombstones(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			createTestTable(t, env.db)

			now := time.Now()
			old := now.Add(-8 * 24 * time.Hour)    // < 7d cutoff → purged
			recent := now.Add(-1 * 24 * time.Hour) // > cutoff → kept
			seed(t, env.db, &old, 5)
			seed(t, env.db, &recent, 3)
			seed(t, env.db, nil, 4) // live → kept

			cutoff := now.Add(-7 * 24 * time.Hour)
			ctx := context.Background()
			cfg := Config{BatchSize: 100, MaxPerPass: 1000, DryRun: false}.sanitized()

			purged, err := purgeTable(ctx, env.db, testTable, cutoff, cfg)
			if err != nil {
				t.Fatalf("purgeTable: %v", err)
			}
			if purged != 5 {
				t.Errorf("purged = %d, want 5", purged)
			}
			if got := countRows(t, env.db, "deleted_at IS NULL"); got != 4 {
				t.Errorf("live rows = %d, want 4", got)
			}
			if got := countRows(t, env.db, "deleted_at IS NOT NULL"); got != 3 {
				t.Errorf("remaining tombstones = %d, want 3 (recent only)", got)
			}
		})
	}
}

func TestPurgeTable_RespectsMaxPerPass(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			createTestTable(t, env.db)

			old := time.Now().Add(-8 * 24 * time.Hour)
			seed(t, env.db, &old, 50)

			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			cfg := Config{BatchSize: 10, MaxPerPass: 20, DryRun: false}.sanitized()

			purged, err := purgeTable(context.Background(), env.db, testTable, cutoff, cfg)
			if err != nil {
				t.Fatalf("purgeTable: %v", err)
			}
			if purged != 20 {
				t.Errorf("purged = %d, want 20 (MaxPerPass cap)", purged)
			}
			if got := countRows(t, env.db, "deleted_at IS NOT NULL"); got != 30 {
				t.Errorf("remaining = %d, want 30 (backlog drains over later ticks)", got)
			}
		})
	}
}

func TestPurgeTable_DryRunDeletesNothing(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			createTestTable(t, env.db)

			old := time.Now().Add(-8 * 24 * time.Hour)
			seed(t, env.db, &old, 7)

			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			cfg := Config{BatchSize: 100, MaxPerPass: 1000, DryRun: true}.sanitized()

			purged, err := purgeTable(context.Background(), env.db, testTable, cutoff, cfg)
			if err != nil {
				t.Fatalf("purgeTable: %v", err)
			}
			if purged != 7 {
				t.Errorf("dry-run purged count = %d, want 7", purged)
			}
			if got := countRows(t, env.db, "deleted_at IS NOT NULL"); got != 7 {
				t.Errorf("dry-run must not delete; remaining = %d, want 7", got)
			}
		})
	}
}

func TestAdvisoryLock_SingleOwnerPerName(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			_ = context.Background() // reserved for future context-aware lock helpers
			name := "tombstone_test_lock_" + driver

			// Owner acquires on one pinned connection.
			_ = env.db.Connection(func(owner *gorm.DB) error {
				got, err := trySessionLock(owner, name)
				if err != nil || !got {
					t.Fatalf("first acquire: got=%v err=%v", got, err)
				}
				// A second pinned connection must NOT acquire the same name.
				_ = env.db.Connection(func(challenger *gorm.DB) error {
					got2, err2 := trySessionLock(challenger, name)
					if err2 != nil {
						t.Fatalf("challenger try: %v", err2)
					}
					if got2 {
						t.Fatalf("challenger acquired a lock already held by owner")
					}
					return nil
				})
				released, rerr := releaseSessionLock(owner, name)
				if rerr != nil || !released {
					t.Fatalf("release: released=%v err=%v", released, rerr)
				}
				return nil
			})

			// After release, a fresh acquire must succeed.
			_ = env.db.Connection(func(next *gorm.DB) error {
				got, err := trySessionLock(next, name)
				if err != nil || !got {
					t.Fatalf("re-acquire after release: got=%v err=%v", got, err)
				}
				_, _ = releaseSessionLock(next, name)
				return nil
			})
		})
	}
}

// TestPurgeTable_DryRunMultiBatchDoesNotInflate guards the dry-run fix: with a
// backlog larger than BatchSize, the reported count must be the true eligible
// count (capped at MaxPerPass), not BatchSize * iterations inflated toward
// MaxPerPass. Before the fix the loop re-selected the same leading rows each
// pass and reported MaxPerPass.
func TestPurgeTable_DryRunMultiBatchDoesNotInflate(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			createTestTable(t, env.db)

			old := time.Now().Add(-8 * 24 * time.Hour)
			seed(t, env.db, &old, 700)

			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			cfg := Config{BatchSize: 500, MaxPerPass: 5000, DryRun: true}.sanitized()

			purged, err := purgeTable(context.Background(), env.db, testTable, cutoff, cfg)
			if err != nil {
				t.Fatalf("purgeTable: %v", err)
			}
			if purged != 700 {
				t.Errorf("dry-run purged = %d, want 700 (true eligible count, not inflated to MaxPerPass)", purged)
			}
			if got := countRows(t, env.db, "deleted_at IS NOT NULL"); got != 700 {
				t.Errorf("dry-run must not delete; remaining = %d, want 700", got)
			}
		})
	}
}

// TestPurgeTable_DryRunReportsFullBacklogBeyondMaxPerPass verifies dry-run
// sizes the WHOLE eligible backlog, not a MaxPerPass-capped sample: a backlog
// larger than MaxPerPass must report its true count (review finding).
func TestPurgeTable_DryRunReportsFullBacklogBeyondMaxPerPass(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			env := newGormEnv(t, driver)
			defer env.teardown()
			createTestTable(t, env.db)

			old := time.Now().Add(-8 * 24 * time.Hour)
			seed(t, env.db, &old, 7000) // larger than MaxPerPass

			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			cfg := Config{BatchSize: 500, MaxPerPass: 5000, DryRun: true}.sanitized()

			purged, err := purgeTable(context.Background(), env.db, testTable, cutoff, cfg)
			if err != nil {
				t.Fatalf("purgeTable: %v", err)
			}
			if purged != 7000 {
				t.Errorf("dry-run purged = %d, want 7000 (full backlog, not capped at MaxPerPass=5000)", purged)
			}
			if got := countRows(t, env.db, "deleted_at IS NOT NULL"); got != 7000 {
				t.Errorf("dry-run must not delete; remaining = %d, want 7000", got)
			}
		})
	}
}

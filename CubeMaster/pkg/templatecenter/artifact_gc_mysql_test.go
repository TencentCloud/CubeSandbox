// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	artifactGCRequireDockerEnv     = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	artifactGCMySQLImage           = "mysql"
	artifactGCMySQLImageTag        = "8.0"
	artifactGCContainerProbeLimit  = 90 * time.Second
	artifactGCTestContextShortWait = 200 * time.Millisecond
)

func TestArtifactGCSessionLockMySQL(t *testing.T) {
	gormDB, sqlDB := newArtifactGCMySQL(t)
	origDB := store.db
	store.db = gormDB
	t.Cleanup(func() { store.db = origDB })

	t.Run("candidate filtering and limit", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		now := time.Now().Unix()
		records := []models.RootfsArtifact{
			{ArtifactID: "failed", Status: ArtifactStatusFailed},
			{ArtifactID: "orphaned", Status: ArtifactStatusOrphaned},
			{ArtifactID: "cleanup-pending", Status: ArtifactStatusCleanupPending},
			{ArtifactID: "expired-ready", Status: ArtifactStatusReady, GCDeadline: now - 1},
			{ArtifactID: "future-ready", Status: ArtifactStatusReady, GCDeadline: now + 3600},
			{ArtifactID: "no-deadline-ready", Status: ArtifactStatusReady},
		}
		if err := gormDB.Create(&records).Error; err != nil {
			t.Fatalf("seed candidates: %v", err)
		}

		got, ok := listArtifactGCCandidatesLocked(context.Background())
		if !ok {
			t.Fatal("expected candidate selection to succeed")
		}
		gotIDs := make([]string, 0, len(got))
		for i := range got {
			gotIDs = append(gotIDs, got[i].ArtifactID)
			if got[i].ID != 0 || got[i].Status != "" || got[i].DownloadToken != "" {
				t.Fatalf("candidate query loaded fields other than artifact_id: %+v", got[i])
			}
		}
		sort.Strings(gotIDs)
		wantIDs := []string{"cleanup-pending", "expired-ready", "failed", "orphaned"}
		if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
			t.Fatalf("candidate IDs = %v, want %v", gotIDs, wantIDs)
		}
		assertArtifactGCLockFree(t, sqlDB)

		resetArtifactGCMySQLRows(t, gormDB)
		many := make([]models.RootfsArtifact, artifactGCMaxPerPass+5)
		for i := range many {
			many[i] = models.RootfsArtifact{
				ArtifactID: fmt.Sprintf("failed-%03d", i),
				Status:     ArtifactStatusFailed,
			}
		}
		if err := gormDB.Create(&many).Error; err != nil {
			t.Fatalf("seed limit candidates: %v", err)
		}
		got, ok = listArtifactGCCandidatesLocked(context.Background())
		if !ok {
			t.Fatal("expected limited candidate selection to succeed")
		}
		if len(got) != artifactGCMaxPerPass {
			t.Fatalf("candidate count = %d, want %d", len(got), artifactGCMaxPerPass)
		}
		assertArtifactGCLockFree(t, sqlDB)
	})

	t.Run("no leak under pool churn", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		stopChurn, waitChurn := startArtifactGCPoolChurn(context.Background(), gormDB)
		defer func() {
			stopChurn()
			if err := waitChurn(); err != nil {
				t.Errorf("pool churn: %v", err)
			}
		}()

		for i := 0; i < 50; i++ {
			if _, ok := listArtifactGCCandidatesLocked(context.Background()); !ok {
				t.Fatalf("round %d: expected lock acquire and list to succeed", i)
			}
			assertArtifactGCLockFree(t, sqlDB)
		}
	})

	t.Run("mutual exclusion", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		conn := artifactGCMySQLConn(t, sqlDB)
		defer conn.Close()
		acquireArtifactGCLock(t, conn)
		held := true
		defer func() {
			if held {
				releaseArtifactGCLock(t, conn)
			}
		}()

		if _, ok := listArtifactGCCandidatesLocked(context.Background()); ok {
			t.Fatal("expected selection to skip while another session holds the lock")
		}
		releaseArtifactGCLock(t, conn)
		held = false

		if _, ok := listArtifactGCCandidatesLocked(context.Background()); !ok {
			t.Fatal("expected selection to recover after competing lock released")
		}
		assertArtifactGCLockFree(t, sqlDB)
	})

	t.Run("cancel after acquire still releases", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		ctx, cancel := context.WithCancel(context.Background())
		const callback = "artifact_gc_test:cancel_before_find"
		if err := gormDB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == constants.RootfsArtifactTableName {
				cancel()
			}
		}); err != nil {
			t.Fatalf("register query callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Query().Remove(callback) }()

		if _, ok := listArtifactGCCandidatesLocked(ctx); ok {
			t.Fatal("expected cancelled candidate query to fail")
		}
		assertArtifactGCLockFullyReleased(t, sqlDB)
	})

	t.Run("query error still releases", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		injected := errors.New("injected artifact candidate query failure")
		const callback = "artifact_gc_test:fail_before_find"
		if err := gormDB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == constants.RootfsArtifactTableName {
				tx.AddError(injected)
			}
		}); err != nil {
			t.Fatalf("register query callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Query().Remove(callback) }()

		_, ok, err := listArtifactGCCandidatesLockedWithError(context.Background())
		if ok {
			t.Fatal("expected injected candidate query failure")
		}
		if !errors.Is(err, injected) {
			t.Fatalf("candidate query error = %v, want %v", err, injected)
		}
		assertArtifactGCLockFullyReleased(t, sqlDB)
	})

	t.Run("acquire SQL error discards pinned session", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		injected := errors.New("injected GET_LOCK failure")
		var connectionID int64
		const callback = "artifact_gc_test:fail_get_lock"
		if err := gormDB.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
			if !strings.Contains(tx.Statement.SQL.String(), "GET_LOCK") {
				return
			}
			connectionID = pinnedArtifactGCConnectionID(t, tx)
			tx.AddError(injected)
		}); err != nil {
			t.Fatalf("register row callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Row().Remove(callback) }()

		_, ok, err := listArtifactGCCandidatesLockedWithError(context.Background())
		if ok || !errors.Is(err, injected) {
			t.Fatalf("acquire result ok=%v err=%v, want injected failure", ok, err)
		}
		assertArtifactGCConnectionGone(t, sqlDB, connectionID)
		assertArtifactGCLockFree(t, sqlDB)
	})

	t.Run("release SQL error discards lock owner", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		injected := errors.New("injected RELEASE_LOCK failure")
		var connectionID int64
		const callback = "artifact_gc_test:fail_release_lock"
		if err := gormDB.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
			if !strings.Contains(tx.Statement.SQL.String(), "RELEASE_LOCK") {
				return
			}
			connectionID = pinnedArtifactGCConnectionID(t, tx)
			tx.AddError(injected)
		}); err != nil {
			t.Fatalf("register row callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Row().Remove(callback) }()

		_, ok, err := listArtifactGCCandidatesLockedWithError(context.Background())
		if ok || !errors.Is(err, injected) {
			t.Fatalf("release result ok=%v err=%v, want injected failure", ok, err)
		}
		assertArtifactGCConnectionGone(t, sqlDB, connectionID)
		assertArtifactGCLockFullyReleased(t, sqlDB)
	})

	t.Run("query and release errors are both preserved", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		queryErr := errors.New("injected combined query failure")
		releaseErr := errors.New("injected combined release failure")
		const queryCallback = "artifact_gc_test:combined_query_failure"
		const rowCallback = "artifact_gc_test:combined_release_failure"
		if err := gormDB.Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
			if tx.Statement.Table == constants.RootfsArtifactTableName {
				tx.AddError(queryErr)
			}
		}); err != nil {
			t.Fatalf("register query callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Query().Remove(queryCallback) }()
		if err := gormDB.Callback().Row().Before("gorm:row").Register(rowCallback, func(tx *gorm.DB) {
			if strings.Contains(tx.Statement.SQL.String(), "RELEASE_LOCK") {
				tx.AddError(releaseErr)
			}
		}); err != nil {
			t.Fatalf("register row callback: %v", err)
		}
		defer func() { _ = gormDB.Callback().Row().Remove(rowCallback) }()

		_, ok, err := listArtifactGCCandidatesLockedWithError(context.Background())
		if ok || !errors.Is(err, queryErr) || !errors.Is(err, releaseErr) {
			t.Fatalf("combined result ok=%v err=%v, want both injected errors", ok, err)
		}
		assertArtifactGCLockFullyReleased(t, sqlDB)
	})

	t.Run("pool acquisition honors caller deadline", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		sqlDB.SetMaxOpenConns(1)
		conn := artifactGCMySQLConn(t, sqlDB)
		defer func() {
			_ = conn.Close()
			sqlDB.SetMaxOpenConns(10)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), artifactGCTestContextShortWait)
		defer cancel()
		started := time.Now()
		if _, ok := listArtifactGCCandidatesLocked(ctx); ok {
			t.Fatal("expected connection acquisition to time out")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("pool acquisition took %s, want a bounded return", elapsed)
		}
	})

	t.Run("release result semantics", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		if err := gormDB.Connection(func(sess *gorm.DB) error {
			released, err := releaseSessionLock(sess, artifactGCLockName)
			if err != nil {
				return err
			}
			if released {
				t.Fatal("expected RELEASE_LOCK to report false/NULL when lock is absent")
			}
			return nil
		}); err != nil {
			t.Fatalf("release absent lock: %v", err)
		}

		owner := artifactGCMySQLConn(t, sqlDB)
		defer owner.Close()
		acquireArtifactGCLock(t, owner)
		defer releaseArtifactGCLock(t, owner)
		if err := gormDB.Connection(func(sess *gorm.DB) error {
			released, err := releaseSessionLock(sess, artifactGCLockName)
			if err != nil {
				return err
			}
			if released {
				t.Fatal("non-owner session unexpectedly released the lock")
			}
			return nil
		}); err != nil {
			t.Fatalf("release lock as non-owner: %v", err)
		}
	})

	t.Run("release session clears stale GORM error", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		if err := gormDB.Connection(func(sess *gorm.DB) error {
			acquired, err := trySessionLock(sess, artifactGCLockName)
			if err != nil {
				return err
			}
			if !acquired {
				t.Fatal("expected test session to acquire lock")
			}
			sess.AddError(errors.New("stale candidate query error"))
			releaseSess := pinnedSessionWithContext(sess, context.Background())
			if releaseSess.Error != nil {
				t.Fatalf("release session inherited stale error: %v", releaseSess.Error)
			}
			released, err := releaseSessionLock(releaseSess, artifactGCLockName)
			if err != nil {
				return err
			}
			if !released {
				t.Fatal("expected clean release session to unlock")
			}
			return nil
		}); err != nil {
			t.Fatalf("release with stale GORM error: %v", err)
		}
		assertArtifactGCLockFree(t, sqlDB)
	})

	t.Run("discard closes a lock-owning session", func(t *testing.T) {
		resetArtifactGCMySQLRows(t, gormDB)
		if err := gormDB.Connection(func(sess *gorm.DB) error {
			acquired, err := trySessionLock(sess, artifactGCLockName)
			if err != nil {
				return err
			}
			if !acquired {
				t.Fatal("expected test session to acquire lock")
			}
			return discardPinnedSession(sess)
		}); err != nil {
			t.Fatalf("discard pinned session: %v", err)
		}
		assertArtifactGCLockFullyReleased(t, sqlDB)
	})
}

func startArtifactGCPoolChurn(parent context.Context, gormDB *gorm.DB) (stop func(), wait func() error) {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	var successes atomic.Int64
	errCh := make(chan error, 1)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					var id int64
					if err := gormDB.WithContext(ctx).Raw("SELECT CONNECTION_ID()").Scan(&id).Error; err != nil {
						if ctx.Err() == nil {
							select {
							case errCh <- err:
							default:
							}
						}
						return
					}
					successes.Add(1)
					// Brief backoff keeps pool pressure without a busy spin.
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()
	}
	return cancel, func() error {
		wg.Wait()
		select {
		case err := <-errCh:
			return err
		default:
		}
		if successes.Load() == 0 {
			return errors.New("pool churn completed without a successful query")
		}
		return nil
	}
}

func resetArtifactGCMySQLRows(t *testing.T, gormDB *gorm.DB) {
	t.Helper()
	if err := gormDB.Exec("DELETE FROM " + constants.RootfsArtifactTableName).Error; err != nil {
		t.Fatalf("reset artifact rows: %v", err)
	}
}

func artifactGCMySQLConn(t *testing.T, sqlDB *sql.DB) *sql.Conn {
	t.Helper()
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("sqlDB.Conn: %v", err)
	}
	return conn
}

func acquireArtifactGCLock(t *testing.T, conn *sql.Conn) {
	t.Helper()
	var got sql.NullInt64
	if err := conn.QueryRowContext(context.Background(),
		"SELECT GET_LOCK(?, 0)", artifactGCLockName).Scan(&got); err != nil {
		t.Fatalf("GET_LOCK: %v", err)
	}
	if !got.Valid || got.Int64 != 1 {
		t.Fatalf("GET_LOCK result = %+v, want 1", got)
	}
}

func releaseArtifactGCLock(t *testing.T, conn *sql.Conn) {
	t.Helper()
	var released sql.NullInt64
	if err := conn.QueryRowContext(context.Background(),
		"SELECT RELEASE_LOCK(?)", artifactGCLockName).Scan(&released); err != nil {
		t.Fatalf("RELEASE_LOCK: %v", err)
	}
	if !released.Valid || released.Int64 != 1 {
		t.Fatalf("RELEASE_LOCK result = %+v, want 1", released)
	}
}

func pinnedArtifactGCConnectionID(t *testing.T, sess *gorm.DB) int64 {
	t.Helper()
	conn, ok := sess.Statement.ConnPool.(*sql.Conn)
	if !ok {
		t.Fatalf("pinned connection pool type = %T, want *sql.Conn", sess.Statement.ConnPool)
	}
	var id int64
	if err := conn.QueryRowContext(context.Background(), "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatalf("query pinned CONNECTION_ID: %v", err)
	}
	return id
}

func assertArtifactGCConnectionGone(t *testing.T, sqlDB *sql.DB, connectionID int64) {
	t.Helper()
	if connectionID == 0 {
		t.Fatal("test did not capture pinned connection ID")
	}
	var count int
	if err := sqlDB.QueryRow(
		"SELECT COUNT(*) FROM information_schema.processlist WHERE ID = ?", connectionID).Scan(&count); err != nil {
		t.Fatalf("query processlist: %v", err)
	}
	if count != 0 {
		t.Fatalf("discarded CONNECTION_ID=%d is still present", connectionID)
	}
}

func assertArtifactGCLockFree(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	if held, owner := artifactGCLockOwner(t, sqlDB); held {
		t.Fatalf("artifact GC lock still held by CONNECTION_ID=%d", owner)
	}
}

func waitForArtifactGCLockFree(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if held, _ := artifactGCLockOwner(t, sqlDB); !held {
			return
		}
		if time.Now().After(deadline) {
			assertArtifactGCLockFree(t, sqlDB)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertArtifactGCLockFullyReleased(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	waitForArtifactGCLockFree(t, sqlDB)
	assertArtifactGCLockCanBeReacquired(t, sqlDB)
}

func artifactGCLockOwner(t *testing.T, sqlDB *sql.DB) (bool, int64) {
	t.Helper()
	var owner sql.NullInt64
	if err := sqlDB.QueryRow("SELECT IS_USED_LOCK(?)", artifactGCLockName).Scan(&owner); err != nil {
		t.Fatalf("IS_USED_LOCK: %v", err)
	}
	return owner.Valid, owner.Int64
}

func assertArtifactGCLockCanBeReacquired(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	conn := artifactGCMySQLConn(t, sqlDB)
	defer conn.Close()
	acquireArtifactGCLock(t, conn)
	releaseArtifactGCLock(t, conn)
}

func newArtifactGCMySQL(t *testing.T) (*gorm.DB, *sql.DB) {
	t.Helper()
	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipArtifactGCDocker(t, "dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipArtifactGCDocker(t, "docker daemon not reachable (%v)", err)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: artifactGCMySQLImage,
		Tag:        artifactGCMySQLImageTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		abortOrSkipArtifactGCDocker(t, "could not start mysql container (%v)", err)
	}
	port := resource.GetPort("3306/tcp")
	dsn := fmt.Sprintf(
		"root:root@tcp(127.0.0.1:%s)/cube_test?charset=utf8&parseTime=true&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s",
		port,
	)
	pool.MaxWait = artifactGCContainerProbeLimit
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

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("sql.Open(mysql): %v", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(10)
	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = sqlDB.Close()
		_ = pool.Purge(resource)
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := gormDB.AutoMigrate(&models.RootfsArtifact{}); err != nil {
		_ = sqlDB.Close()
		_ = pool.Purge(resource)
		t.Fatalf("AutoMigrate RootfsArtifact: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		_ = pool.Purge(resource)
	})
	return gormDB, sqlDB
}

func abortOrSkipArtifactGCDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	required := os.Getenv(artifactGCRequireDockerEnv)
	ci := os.Getenv("CI")
	if required == "1" || strings.EqualFold(required, "true") || ci == "1" || strings.EqualFold(ci, "true") {
		t.Fatalf("%s (fix Docker — required tests may not skip)", msg)
	}
	t.Skip(msg)
}

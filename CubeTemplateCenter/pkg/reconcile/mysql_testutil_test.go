// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package reconcile

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	mysqlTestDSNEnv          = "CUBEMASTER_DAO_TEST_MYSQL_DSN"
	mysqlRequireDockerEnv    = "CUBEMASTER_REQUIRE_DOCKER_TESTS"
	mysqlDockerImage         = "mysql"
	mysqlDockerImageTag      = "8.0"
	mysqlContainerProbeLimit = 90 * time.Second
)

type mysqlDockerEnv struct {
	dsn      string
	teardown func()
}

func requireMySQLDockerTests() bool {
	v := os.Getenv(mysqlRequireDockerEnv)
	if v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	ci := os.Getenv("CI")
	return ci == "true" || ci == "1"
}

func skipOrFailMySQLDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if requireMySQLDockerTests() {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

func newMySQLDockerEnv(t *testing.T) *mysqlDockerEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker-backed MySQL test in short mode")
	}
	if dsn := os.Getenv(mysqlTestDSNEnv); dsn != "" {
		t.Logf("using external MySQL from %s", mysqlTestDSNEnv)
		return &mysqlDockerEnv{dsn: dsn, teardown: func() {}}
	}
	pool, err := dockertest.NewPool("")
	if err != nil {
		skipOrFailMySQLDocker(t, "dockertest not available (%v); set %s", err, mysqlTestDSNEnv)
	}
	if err := pool.Client.Ping(); err != nil {
		skipOrFailMySQLDocker(t, "docker daemon not reachable (%v); set %s", err, mysqlTestDSNEnv)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: mysqlDockerImage,
		Tag:        mysqlDockerImageTag,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=root",
			"MYSQL_DATABASE=cube_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		skipOrFailMySQLDocker(t, "could not start mysql container (%v); set %s", err, mysqlTestDSNEnv)
	}
	port := resource.GetPort("3306/tcp")
	dsn := fmt.Sprintf(
		"root:root@tcp(127.0.0.1:%s)/cube_test?charset=utf8&parseTime=true&loc=Local&timeout=5s&readTimeout=5s&writeTimeout=5s",
		port,
	)
	pool.MaxWait = mysqlContainerProbeLimit
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
	return &mysqlDockerEnv{
		dsn: dsn,
		teardown: func() {
			_ = pool.Purge(resource)
		},
	}
}

func openMySQLGORM(t *testing.T, env *mysqlDockerEnv) *gorm.DB {
	t.Helper()
	sqlDB, err := sql.Open("mysql", env.dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	gormDB, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := gormDB.AutoMigrate(&models.RootfsArtifact{}, &models.ArtifactNodePlacement{}); err != nil {
		_ = sqlDB.Close()
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
		env.teardown()
	})
	return gormDB
}

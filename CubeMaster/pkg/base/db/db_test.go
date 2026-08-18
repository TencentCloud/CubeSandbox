// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package db_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	_ "github.com/tencentcloud/CubeSandbox/CubeDB/dao/driver/postgres"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
)

func TestInitReturnsDaoDefaultOnPostgreSQL(t *testing.T) {
	env := newPostgresTestEnv(t)
	defer env.teardown()

	cfg := dao.Config{
		Driver: "postgres",
		Addr:   env.addr,
		User:   "cube",
		Pwd:    "cube_pass",
		DBName: "cube_test",
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := dao.Open(ctx, cfg); err != nil {
		t.Fatalf("dao.Open: %v", err)
	}
	defer func() { _ = dao.Close() }()

	got := db.Init(&config.DBConfig{Driver: "postgres"})
	if got != dao.Default() {
		t.Fatal("db.Init must return the global dao handle opened by dao.Open")
	}
}

func TestConfigFromDBConfig(t *testing.T) {
	if _, err := db.ConfigFromDBConfig(nil); err == nil {
		t.Fatal("ConfigFromDBConfig(nil) must return an error")
	}

	src := &config.DBConfig{
		Driver:                      "postgres",
		Addr:                        "127.0.0.1:5432",
		User:                        "cube",
		Pwd:                         "cube_pass",
		DBName:                      "cube_test",
		ConnTimeout:                 1,
		ReadTimeout:                 2,
		WriteTimeout:                3,
		MaxIdleConns:                4,
		MaxOpenConns:                5,
		MaxConnLifeTimeSeconds:      6,
		MigrationLockTimeoutSeconds: 7,
	}
	got, err := db.ConfigFromDBConfig(src)
	if err != nil {
		t.Fatalf("ConfigFromDBConfig: %v", err)
	}
	want := dao.Config{
		Driver:                      "postgres",
		Addr:                        "127.0.0.1:5432",
		User:                        "cube",
		Pwd:                         "cube_pass",
		DBName:                      "cube_test",
		ConnTimeoutSeconds:          1,
		ReadTimeoutSeconds:          2,
		WriteTimeoutSeconds:         3,
		MaxIdleConns:                4,
		MaxOpenConns:                5,
		MaxConnLifeTimeSeconds:      6,
		MigrationLockTimeoutSeconds: 7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfigFromDBConfig mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

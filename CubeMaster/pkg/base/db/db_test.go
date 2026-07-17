// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	_ "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao/driver/postgres"
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

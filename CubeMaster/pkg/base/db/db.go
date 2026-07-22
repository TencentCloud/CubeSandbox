// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package db provides database access.
package db

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
)

// Init returns a *gorm.DB for the given DBConfig.
//
// Deprecated: prefer pkg/base/dao.Open / dao.Default directly. This shim is
// kept so the v0.2.2 call-sites in templatecenter / nodemeta / instancecache /
// localcache keep compiling while they are migrated module-by-module.
//
// Init delegates to dao.Open so the deprecated call-sites become
// driver-agnostic (mysql AND postgres) without touching their code. The
// cmd/cubemaster main loop opens the canonical dao handle with the same
// oss/instance DBConfig before any business-package Init runs, and dao.Open is
// idempotent for an identical Driver+DSN: every caller here therefore receives
// the one shared connection pool rather than opening a second, hardcoded one.
//
// Unlike a fresh dao.Open, Init does NOT run schema migrations; the
// cmd/cubemaster main loop is responsible for invoking dao.Migrate exactly once
// at startup before any Init() in business packages.
//
// Historically Init opened its own MySQL pool via sql.Open("mysql", ...). That
// hardcoded the MySQL wire protocol, so pointing CubeMaster at PostgreSQL made
// these four subsystems panic with "invalid connection" even though the dao
// layer itself spoke postgres. Routing through dao.Open removes that binding.
func Init(cfg *config.DBConfig) *gorm.DB {
	db, err := dao.Open(context.Background(), daoConfig(cfg))
	if err == nil {
		return db
	}
	// The canonical handle is already open with a different identity. This
	// happens when ossdb_config and instance_db_config agree on the physical
	// database (driver/addr/db_name — enforced by cmd/cubemaster) but differ in
	// a non-identity field such as user or pool size. The dao layer is a single
	// shared pool by design, so fall back to it rather than opening a second
	// connection (which the original MySQL-hardcoded shim used to do).
	if def := daoDefault(); def != nil {
		CubeLog.Warnf("db.Init: reusing shared dao handle instead of opening a second pool (%v)", err)
		return def
	}
	panic(err)
}

// daoDefault returns the shared dao handle, or nil if dao.Open has never
// succeeded (dao.Default panics in that case, which we translate to nil so the
// caller can surface the original open error instead).
func daoDefault() (db *gorm.DB) {
	defer func() { _ = recover() }()
	return dao.Default()
}

// daoConfig maps a *config.DBConfig onto dao.Config. The field set is identical
// to the mapping cmd/cubemaster uses when it opens the canonical handle, so the
// resulting identity matches and dao.Open returns the shared pool.
func daoConfig(cfg *config.DBConfig) dao.Config {
	return dao.Config{
		Driver:                      cfg.Driver,
		Addr:                        cfg.Addr,
		User:                        cfg.User,
		Pwd:                         cfg.Pwd,
		DBName:                      cfg.DBName,
		ConnTimeoutSeconds:          cfg.ConnTimeout,
		ReadTimeoutSeconds:          cfg.ReadTimeout,
		WriteTimeoutSeconds:         cfg.WriteTimeout,
		MaxIdleConns:                cfg.MaxIdleConns,
		MaxOpenConns:                cfg.MaxOpenConns,
		MaxConnLifeTimeSeconds:      cfg.MaxConnLifeTimeSeconds,
		MigrationLockTimeoutSeconds: cfg.MigrationLockTimeoutSeconds,
		Extra:                       cfg.Extra,
	}
}

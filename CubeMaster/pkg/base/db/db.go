// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package db provides database access.
package db

import (
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"gorm.io/gorm"
)

// Init returns the global dao handle. Retained for backwards compatibility
// with v0.2.2-era callers (nodemeta / localcache / instancecache /
// templatecenter) that still pass a DBConfig. cfg is accepted but ignored —
// the connection is already established by dao.Open before any business
// package Init runs.
func Init(cfg *config.DBConfig) *gorm.DB {
	_ = cfg
	return dao.Default()
}

// ConfigFromDBConfig maps a config.DBConfig to the dao.Config used to open
// the shared database handle. dao.Open keys its idempotence on this config
// identity, so every call site that opens the handle before app startup
// (schema init, integration/mock-debug bootstrap) must build the dao config
// through this helper — keeping the two mappings from drifting.
func ConfigFromDBConfig(src *config.DBConfig) (dao.Config, error) {
	if src == nil {
		return dao.Config{}, errors.New("db config is nil")
	}
	return dao.Config{
		Driver:                      src.Driver,
		Addr:                        src.Addr,
		User:                        src.User,
		Pwd:                         src.Pwd,
		DBName:                      src.DBName,
		ConnTimeoutSeconds:          src.ConnTimeout,
		ReadTimeoutSeconds:          src.ReadTimeout,
		WriteTimeoutSeconds:         src.WriteTimeout,
		MaxIdleConns:                src.MaxIdleConns,
		MaxOpenConns:                src.MaxOpenConns,
		MaxConnLifeTimeSeconds:      src.MaxConnLifeTimeSeconds,
		MigrationLockTimeoutSeconds: src.MigrationLockTimeoutSeconds,
	}, nil
}

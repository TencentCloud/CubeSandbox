// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package db provides database access.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func generateURL(user, password, addr, dbname string, connTimeout, readTimeout, writeTimeout int) string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s)/%s?charset=utf8&parseTime=true&loc=Local&timeout=%ds&readTimeout=%ds&writeTimeout=%ds",
		user, password, addr, dbname, connTimeout, readTimeout, writeTimeout)
}

// Init opens a *gorm.DB against the given DBConfig.
//
// Deprecated: prefer pkg/base/dao.Open / dao.Default. This shim is kept
// so the v0.2.2 call-sites in templatecenter / nodemeta / instancecache /
// localcache keep compiling while they are migrated module-by-module.
//
// Unlike dao.Open, Init does NOT run schema migrations; the cmd/cubemaster
// main loop is responsible for invoking dao.Migrate exactly once at
// startup before any Init() in business packages.
func Init(cfg *config.DBConfig) *gorm.DB {
	db, err := initDB(cfg.User, cfg.Pwd, cfg.Addr, cfg.DBName,
		cfg.ConnTimeout, cfg.ReadTimeout, cfg.WriteTimeout,
		cfg.MaxIdleConns, cfg.MaxOpenConns, cfg.MaxConnLifeTimeSeconds)
	if err != nil {
		panic(err)
	}
	return db
}

func initDB(user, pwd, addr, dbname string, connTimeout, readTimeout, writeTimeout, maxIdleConns, maxOpenConns,
	maxLifeTimeSeconds int) (*gorm.DB, error) {
	url := generateURL(user, pwd, addr, dbname, connTimeout, readTimeout, writeTimeout)
	sqlDB, err := sql.Open("mysql", url)
	if err != nil {
		return nil, err
	}

	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(time.Duration(maxLifeTimeSeconds) * time.Second)
	client, err := gorm.Open(mysql.New(mysql.Config{
		Conn: sqlDB,
	}), &gorm.Config{
		Logger: &Logger{},
	})

	if err != nil {
		return nil, err
	}

	return client, nil
}

type Logger struct {
}

func (ml *Logger) LogMode(logger.LogLevel) logger.Interface {
	return ml
}

func (ml *Logger) Info(ctx context.Context, f string, v ...interface{}) {
	if ctx == nil {
		ctx = context.Background()
	}
	CubeLog.WithContext(ctx).Infof(f, v...)
}

func (ml *Logger) Warn(ctx context.Context, f string, v ...interface{}) {
	if ctx == nil {
		ctx = context.Background()
	}
	CubeLog.WithContext(ctx).Warnf(f, v...)
}

func (ml *Logger) Error(ctx context.Context, f string, v ...interface{}) {
	if ctx == nil {
		ctx = context.Background()
	}
	CubeLog.WithContext(ctx).Errorf(f, v...)
}

func (ml *Logger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
}

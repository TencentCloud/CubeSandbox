// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/cubedb/dao"
)

// Config holds all CubeOps runtime configuration.
type Config struct {
	// Server
	Bind      string
	LogLevel  string
	JWTSecret string

	// Database
	DatabaseURL string

	// JWT
	AccessTTL  time.Duration
	RefreshTTL time.Duration

	// CubeMaster
	CubeMasterAddr string

	// CubeAPI (for SDK endpoint proxy)
	CubeAPIURL string

	// Redis (optional)
	RedisURL string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Bind:           envOr("CUBE_OPS_BIND", "127.0.0.1:3010"),
		LogLevel:       envOr("CUBE_OPS_LOG_LEVEL", "info"),
		JWTSecret:      os.Getenv("JWT_SECRET"),
		DatabaseURL:    envOr("DATABASE_URL", ""),
		CubeMasterAddr: envOr("CUBE_MASTER_ADDR", "http://127.0.0.1:8089"),
		CubeAPIURL:     envOr("CUBE_API_URL", "http://127.0.0.1:3000"),
		RedisURL:       os.Getenv("REDIS_URL"),
	}

	// Build DATABASE_URL from individual env vars if not set directly.
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = buildMySQLURLFromEnv()
	}

	cfg.AccessTTL = parseDurationOr("JWT_ACCESS_TTL", 15*time.Minute)
	cfg.RefreshTTL = parseDurationOr("JWT_REFRESH_TTL", 168*time.Hour)

	// JWT_SECRET is optional — if not set via env, it will be auto-generated
	// and persisted to the DB on first startup (see store.bootstrapJWTSecret).
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL (or CUBE_SANDBOX_MYSQL_* vars) is required")
	}

	return cfg, nil
}

// DaoConfig converts the CubeOps config to a cubedb dao.Config.
func (c *Config) DaoConfig() dao.Config {
	user, pass, addr, dbname := parseMySQLURL(c.DatabaseURL)
	return dao.Config{
		Driver:     "mysql",
		User:       user,
		Pwd:        pass,
		Addr:       addr,
		DBName:     dbname,
		MaxIdleConns: 10,
		MaxOpenConns: 100,
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func buildMySQLURLFromEnv() string {
	host := os.Getenv("CUBE_SANDBOX_MYSQL_HOST")
	if host == "" {
		return ""
	}
	port := envOr("CUBE_SANDBOX_MYSQL_PORT", "3306")
	user := os.Getenv("CUBE_SANDBOX_MYSQL_USER")
	pass := os.Getenv("CUBE_SANDBOX_MYSQL_PASSWORD")
	db := os.Getenv("CUBE_SANDBOX_MYSQL_DB")
	return fmt.Sprintf("mysql://%s:%s@%s:%s/%s", user, pass, host, port, db)
}

func parseMySQLURL(url string) (user, pass, addr, dbname string) {
	// Strip mysql:// prefix
	s := strings.TrimPrefix(url, "mysql://")
	// Format: user:pass@host:port/db
	atIdx := strings.LastIndex(s, "@")
	if atIdx < 0 {
		return
	}
	userpass := s[:atIdx]
	hostdb := s[atIdx+1:]

	colonIdx := strings.Index(userpass, ":")
	if colonIdx >= 0 {
		user = userpass[:colonIdx]
		pass = userpass[colonIdx+1:]
	} else {
		user = userpass
	}

	slashIdx := strings.Index(hostdb, "/")
	if slashIdx >= 0 {
		addr = hostdb[:slashIdx]
		dbname = hostdb[slashIdx+1:]
	} else {
		addr = hostdb
	}
	return
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package config loads CubeOps configuration.
//
// Resolution order, highest priority first:
//
//  1. Environment variables (CUBE_OPS_*, DATABASE_URL, JWT_SECRET, ...).
//     This keeps the existing deployment workflow working: systemd / k8s
//     manifests keep using env vars without changes.
//
//  2. YAML file at the path in CUBE_OPS_CONFIG (or /etc/cube/ops.yaml if
//     unset). YAML is the recommended way to configure CubeOps going forward
//     because it groups all knobs in one place and supports comments.
//
//  3. Built-in defaults.
//
// The YAML schema is intentionally flat — one section per top-level
// component. See config.example.yaml for a fully commented example.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
)

// Config holds all CubeOps runtime configuration.
type Config struct {
	// Server
	Bind      string `yaml:"bind"`
	LogLevel  string `yaml:"log_level"`
	JWTSecret string `yaml:"jwt_secret"`

	// Database — either a single URL or the individual fields below.
	DatabaseURL   string `yaml:"database_url"`
	MySQLHost     string `yaml:"mysql_host"`
	MySQLPort     int    `yaml:"mysql_port"`
	MySQLUser     string `yaml:"mysql_user"`
	MySQLPassword string `yaml:"mysql_password"`
	MySQLDB       string `yaml:"mysql_db"`

	// JWT
	AccessTTL  time.Duration `yaml:"access_ttl"`
	RefreshTTL time.Duration `yaml:"refresh_ttl"`

	// CubeMaster
	CubeMasterAddr string `yaml:"cubemaster_addr"`

	// CubeAPI (for SDK endpoint proxy)
	CubeAPIURL string `yaml:"cubeapi_url"`

	// Redis (optional)
	RedisURL string `yaml:"redis_url"`

	// Sandbox domain exposed to SDK clients; matches SDK handler's
	// CUBE_API_SANDBOX_DOMAIN env so the /config endpoint stays in sync.
	SandboxDomain string `yaml:"sandbox_domain"`
}

// Load reads configuration from YAML + environment variables (env wins).
func Load() (*Config, error) {
	cfg, err := loadFromYAML()
	if err != nil {
		return nil, err
	}

	// Environment variable overrides take precedence.
	overrideFromEnv(cfg)

	// Build DATABASE_URL from individual fields if not set directly.
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = cfg.buildMySQLURL()
	}

	// Default durations.
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 168 * time.Hour
	}
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1:3010"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.CubeMasterAddr == "" {
		cfg.CubeMasterAddr = "http://127.0.0.1:8089"
	}
	if cfg.CubeAPIURL == "" {
		cfg.CubeAPIURL = "http://127.0.0.1:3000"
	}
	if cfg.SandboxDomain == "" {
		cfg.SandboxDomain = "cube.app"
	}

	// JWT_SECRET is optional — if not set, it will be auto-generated and
	// persisted to the DB on first startup (see store.bootstrapJWTSecret).
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("database_url (or mysql_host + mysql_user + mysql_password + mysql_db) is required (set in YAML %s or via DATABASE_URL env)",
			yamlConfigPath())
	}

	return cfg, nil
}

// DaoConfig converts the CubeOps config to a CubeDB dao.Config.
//
// If DatabaseURL is set, it is the single source of truth and the individual
// MySQL* fields are ignored. This fixes R06: previously DatabaseURL was
// accepted by Load() and passed the required-field check, but DaoConfig()
// silently used the (possibly empty) MySQL* fields instead, causing CubeOps
// to connect with empty user/db or fall back to localhost.
//
// S6 fix: the driver is selected from the URL scheme (mysql:// or
// postgres://), so PostgreSQL deployments work instead of silently falling
// back to MySQL and failing on dialect-specific SQL.
func (c *Config) DaoConfig() dao.Config {
	// Fast path: no DatabaseURL — use the individual fields as before.
	if c.DatabaseURL == "" {
		return dao.Config{
			Driver:       "mysql",
			User:         c.MySQLUser,
			Pwd:          c.MySQLPassword,
			Addr:         fmt.Sprintf("%s:%d", c.MySQLHost, c.MySQLPortOrDefault()),
			DBName:       c.MySQLDB,
			MaxIdleConns: 10,
			MaxOpenConns: 100,
		}
	}

	// Parse DatabaseURL and select driver from the scheme.
	// Supported schemes: mysql://, postgres:// (or postgresql://).
	driver, user, pass, host, port, dbname := parseDatabaseURL(c.DatabaseURL)
	return dao.Config{
		Driver:       driver,
		User:         user,
		Pwd:          pass,
		Addr:         fmt.Sprintf("%s:%d", host, port),
		DBName:       dbname,
		MaxIdleConns: 10,
		MaxOpenConns: 100,
	}
}

// parseDatabaseURL extracts (driver, user, password, host, port, dbname) from
// a database URL. The driver is inferred from the scheme:
//   - mysql://    → "mysql"
//   - postgres:// or postgresql:// → "postgres"
//
// If parsing fails for any component, the caller's individual fields are NOT
// consulted — the error surfaces as an empty component that the DB driver
// will reject with a clear "access denied" or "unknown database" message,
// which is better than silently connecting to the wrong database.
func parseDatabaseURL(rawURL string) (driver, user, pass, host string, port int, dbname string) {
	port = 3306 // default (MySQL)

	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	// Select driver from scheme.
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "postgres", "postgresql":
		driver = "postgres"
		port = 5432 // default PG port if not specified
	case "mysql", "":
		driver = "mysql"
	default:
		driver = scheme // let resolveDriver reject unknown schemes
	}

	// url.Parse puts user:pass into User, host:port into Host.
	if u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			pass = p
		}
	}

	host = u.Hostname()
	if h := u.Port(); h != "" {
		if p, err := strconv.Atoi(h); err == nil {
			port = p
		}
	}

	// Database name is the path without leading "/".
	dbname = strings.TrimPrefix(u.Path, "/")

	return
}

// MySQLPortOrDefault returns the configured MySQL port or 3306.
func (c *Config) MySQLPortOrDefault() int {
	if c.MySQLPort == 0 {
		return 3306
	}
	return c.MySQLPort
}

// buildMySQLURL builds a mysql:// URL from the individual MySQL fields.
func (c *Config) buildMySQLURL() string {
	if c.MySQLHost == "" {
		return ""
	}
	return fmt.Sprintf("mysql://%s:%s@%s:%d/%s",
		c.MySQLUser, c.MySQLPassword, c.MySQLHost, c.MySQLPortOrDefault(), c.MySQLDB)
}

func yamlConfigPath() string {
	if p := os.Getenv("CUBE_OPS_CONFIG"); p != "" {
		return p
	}
	return "/etc/cube/ops.yaml"
}

// loadFromYAML reads config from the YAML file. If the file does not exist,
// the returned config is the zero value (env vars / defaults fill in).
// An existing-but-malformed file is a hard error.
func loadFromYAML() (*Config, error) {
	cfg := &Config{}
	path := yamlConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	return cfg, nil
}

// overrideFromEnv fills in any zero-valued fields from environment
// variables. Env vars are higher priority than the YAML file.
func overrideFromEnv(cfg *Config) {
	if v := os.Getenv("CUBE_OPS_BIND"); v != "" {
		cfg.Bind = v
	}
	if v := os.Getenv("CUBE_OPS_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.DatabaseURL = v
	}
	if v := os.Getenv("CUBE_SANDBOX_MYSQL_HOST"); v != "" {
		cfg.MySQLHost = v
	}
	if v := os.Getenv("CUBE_SANDBOX_MYSQL_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil {
			cfg.MySQLPort = p
		}
	}
	if v := os.Getenv("CUBE_SANDBOX_MYSQL_USER"); v != "" {
		cfg.MySQLUser = v
	}
	if v := os.Getenv("CUBE_SANDBOX_MYSQL_PASSWORD"); v != "" {
		cfg.MySQLPassword = v
	}
	if v := os.Getenv("CUBE_SANDBOX_MYSQL_DB"); v != "" {
		cfg.MySQLDB = v
	}
	if v := os.Getenv("CUBE_MASTER_ADDR"); v != "" {
		cfg.CubeMasterAddr = v
	}
	if v := os.Getenv("CUBE_API_URL"); v != "" {
		cfg.CubeAPIURL = v
	}
	if v := os.Getenv("REDIS_URL"); v != "" {
		cfg.RedisURL = v
	}
	if v := os.Getenv("CUBE_API_SANDBOX_DOMAIN"); v != "" {
		cfg.SandboxDomain = v
	}
	if v := os.Getenv("JWT_ACCESS_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.AccessTTL = d
		}
	}
	if v := os.Getenv("JWT_REFRESH_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.RefreshTTL = d
		}
	}
}

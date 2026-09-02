// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
)

// mustDaoConfig calls DaoConfig and fails the test on a config error.
func mustDaoConfig(t *testing.T, cfg *Config) dao.Config {
	t.Helper()
	dc, err := cfg.DaoConfig()
	if err != nil {
		t.Fatalf("DaoConfig: %v", err)
	}
	return dc
}

// TestDaoConfig_DatabaseURLWins proves R06: when DatabaseURL is set,
// DaoConfig() parses it and produces a dao.Config whose User/Pwd/Addr/DBName
// come ENTIRELY from the URL — the individual MySQL* fields are ignored even
// if they are empty or hold conflicting values.
func TestDaoConfig_DatabaseURLWins(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://alice:s3cret@10.0.0.5:3307/mydb",
	}

	dc := mustDaoConfig(t, cfg)

	if dc.Driver != "mysql" {
		t.Errorf("Driver = %q, want mysql", dc.Driver)
	}
	if dc.User != "alice" {
		t.Errorf("User = %q, want alice (from URL)", dc.User)
	}
	if dc.Pwd != "s3cret" {
		t.Errorf("Pwd = %q, want s3cret (from URL)", dc.Pwd)
	}
	if dc.Addr != "10.0.0.5:3307" {
		t.Errorf("Addr = %q, want 10.0.0.5:3307 (from URL)", dc.Addr)
	}
	if dc.DBName != "mydb" {
		t.Errorf("DBName = %q, want mydb (from URL)", dc.DBName)
	}
}

// TestDaoConfig_DatabaseURLWinsOverConflictingMySQLFields proves the URL wins
// even when the MySQL* fields are populated with different values: no field
// mixing between the two forms.
func TestDaoConfig_DatabaseURLWinsOverConflictingMySQLFields(t *testing.T) {
	cfg := &Config{
		DatabaseURL:   "mysql://alice:s3cret@10.0.0.5:3307/mydb",
		MySQLHost:     "wrong-host",
		MySQLPort:     9999,
		MySQLUser:     "wrong-user",
		MySQLPassword: "wrong-pass",
		MySQLDB:       "wrong-db",
	}

	dc := mustDaoConfig(t, cfg)

	if dc.User != "alice" {
		t.Errorf("User = %q, want alice (URL must win over MySQLUser)", dc.User)
	}
	if dc.Pwd != "s3cret" {
		t.Errorf("Pwd = %q, want s3cret (URL must win over MySQLPassword)", dc.Pwd)
	}
	if dc.Addr != "10.0.0.5:3307" {
		t.Errorf("Addr = %q, want 10.0.0.5:3307 (URL must win over MySQLHost/Port)", dc.Addr)
	}
	if dc.DBName != "mydb" {
		t.Errorf("DBName = %q, want mydb (URL must win over MySQLDB)", dc.DBName)
	}
}

// TestDaoConfig_FallsBackToMySQLFields proves that when DatabaseURL is NOT set,
// the individual MySQL* fields are used (backward compatibility with
// deployments that configure via CUBE_SANDBOX_MYSQL_* env vars).
func TestDaoConfig_FallsBackToMySQLFields(t *testing.T) {
	cfg := &Config{
		MySQLHost:     "db.internal",
		MySQLPort:     3306,
		MySQLUser:     "svc",
		MySQLPassword: "svcpass",
		MySQLDB:       "svcdb",
	}

	dc := mustDaoConfig(t, cfg)

	if dc.Driver != "mysql" {
		t.Errorf("Driver = %q, want mysql", dc.Driver)
	}
	if dc.User != "svc" {
		t.Errorf("User = %q, want svc", dc.User)
	}
	if dc.Pwd != "svcpass" {
		t.Errorf("Pwd = %q, want svcpass", dc.Pwd)
	}
	if dc.Addr != "db.internal:3306" {
		t.Errorf("Addr = %q, want db.internal:3306", dc.Addr)
	}
	if dc.DBName != "svcdb" {
		t.Errorf("DBName = %q, want svcdb", dc.DBName)
	}
}

// TestDaoConfig_FieldPathMissingField_FailsFast asserts the field path fails
// fast on a missing host, user or database, like the URL path.
func TestDaoConfig_FieldPathMissingField_FailsFast(t *testing.T) {
	cases := []struct {
		name, want string
		mut        func(*Config)
	}{
		{"missing host", "mysql_host", func(c *Config) { c.MySQLHost = "" }},
		{"missing user", "mysql_user", func(c *Config) { c.MySQLUser = "" }},
		{"missing db", "mysql_db", func(c *Config) { c.MySQLDB = "" }},
		{"blank user", "mysql_user", func(c *Config) { c.MySQLUser = "  " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				MySQLHost:     "db.internal",
				MySQLPort:     3306,
				MySQLUser:     "svc",
				MySQLPassword: "svcpass",
				MySQLDB:       "svcdb",
			}
			tc.mut(cfg)
			if _, err := cfg.DaoConfig(); err == nil {
				t.Errorf("DaoConfig() = nil err, want error mentioning %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("DaoConfig() error = %v, want to mention %q", err, tc.want)
			}
		})
	}
}

// TestDaoConfig_SpecialCharsInFieldPassword is the issue #1559 regression:
// a password containing URL-reserved characters must still reach dao.Config
// unchanged when the connection is configured from the individual MySQL*
// fields (no intermediate URL is built).
func TestDaoConfig_SpecialCharsInFieldPassword(t *testing.T) {
	passwords := []string{
		"p#a?b/c",
		"p@ssword",
		"p%zzword", // invalid %-escape would break a hand-built URL
		"pa ss:word",
		"p\\word;quote'",
	}
	for _, pwd := range passwords {
		t.Run(pwd, func(t *testing.T) {
			cfg := &Config{
				MySQLHost:     "external-mysql-host",
				MySQLPort:     13307,
				MySQLUser:     "cubeuser",
				MySQLPassword: pwd,
				MySQLDB:       "cubedb",
			}
			dc := mustDaoConfig(t, cfg)
			if dc.Addr != "external-mysql-host:13307" {
				t.Errorf("Addr = %q, want external-mysql-host:13307", dc.Addr)
			}
			if dc.Pwd != pwd {
				t.Errorf("Pwd = %q, want %q", dc.Pwd, pwd)
			}
			if dc.User != "cubeuser" || dc.DBName != "cubedb" {
				t.Errorf("User/DBName corrupted: %q/%q", dc.User, dc.DBName)
			}
		})
	}
}

// TestDaoConfig_DefaultPort proves that a URL without an explicit port
// defaults to 3306 — a common omission in DATABASE_URL strings.
func TestDaoConfig_DefaultPort(t *testing.T) {
	cfg := &Config{
		DatabaseURL: "mysql://alice:s3cret@db.internal/mydb",
	}

	dc := mustDaoConfig(t, cfg)

	if dc.Addr != "db.internal:3306" {
		t.Errorf("Addr = %q, want db.internal:3306 (default port)", dc.Addr)
	}
}

// TestDaoConfig_MalformedURL_FailsFast proves a broken DATABASE_URL surfaces a
// clear error instead of silently falling back to localhost:3306.
func TestDaoConfig_MalformedURL_FailsFast(t *testing.T) {
	bad := []string{
		"mysql://user:p#a@bad:3306/db", // '#' begins a fragment: empty host
		"://missing-scheme/db",
		"mysql://@10.0.0.1:3306/db",  // no user
		"mysql://u:p@10.0.0.1:3306/", // no database
		"mysql://u:p@:3306/db",       // no host
		"ftp://u:p@10.0.0.1:3306/db",
	}
	for _, raw := range bad {
		cfg := &Config{DatabaseURL: raw}
		if _, err := cfg.DaoConfig(); err == nil {
			t.Errorf("DaoConfig(%q) = nil err, want error", raw)
		}
	}
}

// TestDaoConfig_OverflowPortFailsFast asserts an int64-overflowing port hits
// the Atoi "invalid port" branch, since url.Parse accepts pure-digit ports.
func TestDaoConfig_OverflowPortFailsFast(t *testing.T) {
	cfg := &Config{DatabaseURL: "mysql://u:p@10.0.0.1:99999999999999999999/db"}
	_, err := cfg.DaoConfig()
	if err == nil {
		t.Fatal("DaoConfig() = nil err, want error for overflow port")
	}
	if !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("error = %v, want to mention 'invalid port'", err)
	}
}

// TestDaoConfig_PoolLimits asserts the URL and field paths both set pool limits.
func TestDaoConfig_PoolLimits(t *testing.T) {
	urlCfg := &Config{DatabaseURL: "mysql://u:p@h:3306/db"}
	dc := mustDaoConfig(t, urlCfg)
	if dc.MaxIdleConns != 10 || dc.MaxOpenConns != 100 {
		t.Errorf("URL path pool limits = idle %d open %d, want 10/100", dc.MaxIdleConns, dc.MaxOpenConns)
	}

	fieldCfg := &Config{MySQLHost: "h", MySQLUser: "u", MySQLPassword: "p", MySQLDB: "db"}
	dc = mustDaoConfig(t, fieldCfg)
	if dc.MaxIdleConns != 10 || dc.MaxOpenConns != 100 {
		t.Errorf("field path pool limits = idle %d open %d, want 10/100", dc.MaxIdleConns, dc.MaxOpenConns)
	}
}

// TestDaoConfig_ErrorRedactsPassword ensures a bad DATABASE_URL never echoes
// the password in its error.
func TestDaoConfig_ErrorRedactsPassword(t *testing.T) {
	cases := []string{
		"mysql://user:secret#frag@10.0.0.1:3306/db", // parse issue: '#' -> fragment
		"mysql://user:secret@10.0.0.1:badport/db",   // invalid port
		"mysql://user:secret@/db",                   // empty host
		"mysql:user:secret@host",                    // opaque: credentials Redacted() can't mask
		"mysql://@10.0.0.1:3306/db",                 // empty user
		"mysql://user:secret@10.0.0.1:3306/",        // empty database
	}
	for _, raw := range cases {
		cfg := &Config{DatabaseURL: raw}
		_, err := cfg.DaoConfig()
		if err == nil {
			t.Errorf("DaoConfig(%q) = nil err, want error", raw)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("DaoConfig(%q) error leaks password: %v", raw, err)
		}
	}
}

// TestDaoConfig_FullLoadToDaoConfig proves the end-to-end data flow: Load()
// accepts a YAML with only database_url, and DaoConfig() correctly translates
// it — no field is lost between config loading and the dao.Config handed to
// store.New().
func TestDaoConfig_FullLoadToDaoConfig(t *testing.T) {
	dir := t.TempDir()
	yamlPath := dir + "/config.yaml"
	yamlContent := []byte("database_url: \"mysql://loader:loaderpass@192.168.1.10:3306/loaderdb\"\n")
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dc := mustDaoConfig(t, cfg)

	if dc.User != "loader" {
		t.Errorf("User = %q, want loader", dc.User)
	}
	if dc.Pwd != "loaderpass" {
		t.Errorf("Pwd = %q, want loaderpass", dc.Pwd)
	}
	if dc.Addr != "192.168.1.10:3306" {
		t.Errorf("Addr = %q, want 192.168.1.10:3306", dc.Addr)
	}
	if dc.DBName != "loaderdb" {
		t.Errorf("DBName = %q, want loaderdb", dc.DBName)
	}
}

// TestLoad_MysqlFieldsFromEnv produces a dao.Config from CUBE_SANDBOX_MYSQL_*
// env vars (the Helm mysql.* and TKE deployment path). A password with
// URL-reserved characters must round-trip intact — regression for #1559.
func TestLoad_MysqlFieldsFromEnv(t *testing.T) {
	t.Setenv("CUBE_OPS_CONFIG", "/nonexistent/path/config.yaml")
	t.Setenv("CUBE_SANDBOX_MYSQL_HOST", "external-mysql-host")
	t.Setenv("CUBE_SANDBOX_MYSQL_PORT", "13307")
	t.Setenv("CUBE_SANDBOX_MYSQL_USER", "cubeuser")
	t.Setenv("CUBE_SANDBOX_MYSQL_PASSWORD", "p#a?b/c")
	t.Setenv("CUBE_SANDBOX_MYSQL_DB", "cubedb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dc := mustDaoConfig(t, cfg)

	if dc.Addr != "external-mysql-host:13307" {
		t.Errorf("Addr = %q, want external-mysql-host:13307", dc.Addr)
	}
	if dc.Pwd != "p#a?b/c" {
		t.Errorf("Pwd = %q, want p#a?b/c", dc.Pwd)
	}
	if dc.User != "cubeuser" || dc.DBName != "cubedb" {
		t.Errorf("User/DBName corrupted: %q/%q", dc.User, dc.DBName)
	}
}

// TestLoad_MysqlFieldsFromYAML produces a dao.Config from the individual YAML
// mysql_* keys (the file-based path of #1559). URL-reserved characters in the
// password must round-trip intact.
func TestLoad_MysqlFieldsFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlPath := dir + "/config.yaml"
	yamlContent := []byte(`mysql_host: "external-mysql-host"
mysql_port: 13307
mysql_user: "cubeuser"
mysql_password: "p#a?b/c"
mysql_db: "cubedb"
`)
	if err := os.WriteFile(yamlPath, yamlContent, 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	t.Setenv("CUBE_OPS_CONFIG", yamlPath)
	// Clear any ambient env vars that would override the YAML fields.
	for _, k := range []string{"DATABASE_URL", "CUBE_SANDBOX_MYSQL_HOST", "CUBE_SANDBOX_MYSQL_PORT", "CUBE_SANDBOX_MYSQL_USER", "CUBE_SANDBOX_MYSQL_PASSWORD", "CUBE_SANDBOX_MYSQL_DB"} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dc := mustDaoConfig(t, cfg)

	if dc.Addr != "external-mysql-host:13307" {
		t.Errorf("Addr = %q, want external-mysql-host:13307", dc.Addr)
	}
	if dc.Pwd != "p#a?b/c" {
		t.Errorf("Pwd = %q, want p#a?b/c", dc.Pwd)
	}
	if dc.User != "cubeuser" || dc.DBName != "cubedb" {
		t.Errorf("User/DBName corrupted: %q/%q", dc.User, dc.DBName)
	}
}

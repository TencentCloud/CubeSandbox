// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
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

// TestDaoConfig_NoDBConfig_Fails asserts an empty config errors out with a
// message naming the real required knobs and the config file path.
func TestDaoConfig_NoDBConfig_Fails(t *testing.T) {
	_, err := (&Config{}).DaoConfig()
	if err == nil {
		t.Fatal("DaoConfig() on empty config = nil err, want error")
	}
	for _, want := range []string{
		"DATABASE_URL",
		"CUBE_SANDBOX_MYSQL_{HOST,USER,DB}",
		"PASSWORD optional",
		yamlConfigPath(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("DaoConfig() error = %q, want to contain %q", err, want)
		}
	}
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

// TestDaoConfig_UsesMySQLFieldsWhenNoURL proves the MySQL* fields are used
// when DatabaseURL is unset.
func TestDaoConfig_UsesMySQLFieldsWhenNoURL(t *testing.T) {
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

// TestDaoConfig_WhitespaceURL_UsesMySQLFields proves a whitespace-only
// DATABASE_URL counts as unset, so the split MySQL* fields are used.
func TestDaoConfig_WhitespaceURL_UsesMySQLFields(t *testing.T) {
	for _, url := range []string{" ", "   ", "\t"} {
		t.Run("url="+strconv.Quote(url), func(t *testing.T) {
			cfg := &Config{
				DatabaseURL:   url,
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
				t.Errorf("User = %q, want svc (split-field path)", dc.User)
			}
			if dc.Addr != "db.internal:3306" {
				t.Errorf("Addr = %q, want db.internal:3306", dc.Addr)
			}
			if dc.DBName != "svcdb" {
				t.Errorf("DBName = %q, want svcdb", dc.DBName)
			}
		})
	}
}

// TestDaoConfig_WhitespacePaddedURL_IsTrimmed proves a URL with stray
// leading/trailing whitespace (mis-quoted YAML scalar, env value with a
// trailing newline) is trimmed and parsed rather than rejected.
func TestDaoConfig_WhitespacePaddedURL_IsTrimmed(t *testing.T) {
	for _, raw := range []string{
		" mysql://alice:s3cret@10.0.0.5:3307/mydb",
		"mysql://alice:s3cret@10.0.0.5:3307/mydb\n",
		"\tmysql://alice:s3cret@10.0.0.5:3307/mydb ",
	} {
		t.Run("url="+strconv.Quote(raw), func(t *testing.T) {
			dc := mustDaoConfig(t, &Config{DatabaseURL: raw})
			if dc.Driver != "mysql" {
				t.Errorf("Driver = %q, want mysql", dc.Driver)
			}
			if dc.Addr != "10.0.0.5:3307" {
				t.Errorf("Addr = %q, want 10.0.0.5:3307", dc.Addr)
			}
			if dc.User != "alice" || dc.Pwd != "s3cret" || dc.DBName != "mydb" {
				t.Errorf("User/Pwd/DBName = %q/%q/%q, want alice/s3cret/mydb", dc.User, dc.Pwd, dc.DBName)
			}
		})
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

// TestDaoConfig_SpecialCharsInFieldPassword covers the direct field-path copy;
// the end-to-end regression gates are TestLoad_MysqlFieldsFromEnv /
// TestLoad_MysqlFieldsFromYAML.
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

// TestDaoConfig_BracketedHostIsNormalized proves a host copied out of a URL
// in bracketed IPv6 form ([2001:db8::1]) is normalized before JoinHostPort,
// instead of being double-bracketed into an invalid address.
func TestDaoConfig_BracketedHostIsNormalized(t *testing.T) {
	cfg := &Config{
		MySQLHost:     "[2001:db8::1]",
		MySQLPort:     3306,
		MySQLUser:     "svc",
		MySQLPassword: "svcpass",
		MySQLDB:       "svcdb",
	}
	dc := mustDaoConfig(t, cfg)
	if dc.Addr != "[2001:db8::1]:3306" {
		t.Errorf("Addr = %q, want [2001:db8::1]:3306", dc.Addr)
	}

	// Brackets alone must not slip past the empty-host check: JoinHostPort
	// would emit ":3306", which normalizeAddr silently localises.
	cfg = &Config{MySQLHost: "[]", MySQLUser: "svc", MySQLDB: "svcdb"}
	if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "mysql_host") {
		t.Errorf("DaoConfig() on host %q = %v, want mysql_host error", "[]", err)
	}
}

// TestDaoConfig_HostWithPortFailsFast proves a host field carrying a port
// (host:3306) is rejected instead of producing a garbage address.
func TestDaoConfig_HostWithPortFailsFast(t *testing.T) {
	cfg := &Config{MySQLHost: "10.0.0.1:3306", MySQLUser: "svc", MySQLDB: "svcdb"}
	if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "must not include a port") {
		t.Errorf("DaoConfig() = %v, want 'must not include a port' error", err)
	}
}

// TestDaoConfig_FieldPortOutOfRangeFailsFast proves the split-field path
// rejects ports outside 1-65535 instead of surfacing an opaque dial error.
func TestDaoConfig_FieldPortOutOfRangeFailsFast(t *testing.T) {
	for _, port := range []int{99999, -1} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			cfg := &Config{MySQLHost: "db.internal", MySQLPort: port, MySQLUser: "svc", MySQLDB: "svcdb"}
			if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "mysql_port") {
				t.Errorf("DaoConfig() = %v, want mysql_port range error", err)
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
	cases := []struct {
		raw, want string
	}{
		{"mysql://@10.0.0.1:3306/db", "no user"},
		{"mysql://u:p@10.0.0.1:3306/", "no database name"},
		{"mysql://u:p@:3306/db", "no host"},
		{"ftp://u:p@10.0.0.1:3306/db", `unsupported database_url scheme "ftp"`},
		// Rejected by url.Parse first, so only the generic parse error is seen.
		{"mysql://user:p#a@bad:3306/db", "failed to parse"},
		{"mysql://u:p@10.0.0.1:badport/db", "failed to parse"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if _, err := (&Config{DatabaseURL: tc.raw}).DaoConfig(); err == nil {
				t.Fatalf("DaoConfig() = nil err, want error containing %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("DaoConfig() error = %v, want containing %q", err, tc.want)
			}
		})
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

// TestDaoConfig_URLPortOutOfRangeFailsFast proves the URL path rejects ports
// outside 1-65535 at config time instead of failing at dial time.
func TestDaoConfig_URLPortOutOfRangeFailsFast(t *testing.T) {
	for _, raw := range []string{
		"mysql://u:p@10.0.0.1:0/db",
		"mysql://u:p@10.0.0.1:99999/db",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg := &Config{DatabaseURL: raw}
			if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "invalid port") {
				t.Errorf("DaoConfig() = %v, want 'invalid port' error", err)
			}
		})
	}
}

// TestDaoConfig_URLSSLModeIsHonored proves a postgres URL's sslmode reaches
// the driver via Extra instead of being silently dropped (the driver defaults
// to sslmode=disable, so dropping it meant connecting in plaintext).
func TestDaoConfig_URLSSLModeIsHonored(t *testing.T) {
	dc := mustDaoConfig(t, &Config{DatabaseURL: "postgres://alice:s3cret@10.0.0.5:5432/mydb?sslmode=require"})
	if dc.Driver != "postgres" {
		t.Errorf("Driver = %q, want postgres", dc.Driver)
	}
	if dc.Extra["sslmode"] != "require" {
		t.Errorf("Extra[sslmode] = %q, want require", dc.Extra["sslmode"])
	}
	if dc.Addr != "10.0.0.5:5432" || dc.DBName != "mydb" {
		t.Errorf("Addr/DBName = %q/%q, want 10.0.0.5:5432/mydb", dc.Addr, dc.DBName)
	}
}

// TestDaoConfig_URLUnknownQueryParamFailsFast proves query parameters other
// than postgres sslmode are rejected instead of silently ignored — including
// sslmode on a mysql URL, where it never applied.
func TestDaoConfig_URLUnknownQueryParamFailsFast(t *testing.T) {
	for _, raw := range []string{
		"mysql://u:p@10.0.0.1:3306/db?connect_timeout=10",
		"mysql://u:p@10.0.0.1:3306/db?sslmode=require",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg := &Config{DatabaseURL: raw}
			if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "unsupported query parameter") {
				t.Errorf("DaoConfig() = %v, want 'unsupported query parameter' error", err)
			}
		})
	}
}

// TestDaoConfig_URLFragmentFailsFast proves a URL fragment is rejected
// instead of silently dropped.
func TestDaoConfig_URLFragmentFailsFast(t *testing.T) {
	cfg := &Config{DatabaseURL: "mysql://u:p@10.0.0.1:3306/db#x"}
	if _, err := cfg.DaoConfig(); err == nil || !strings.Contains(err.Error(), "unsupported fragment") {
		t.Errorf("DaoConfig() = %v, want 'unsupported fragment' error", err)
	}
}

// TestDaoConfig_URLPasswordIsDecoded pins the URL contract: a percent-encoded
// password is decoded to its literal value (p%23a%3Fb%2Fc → p#a?b/c), the
// encoding operators must use for URL-reserved characters.
func TestDaoConfig_URLPasswordIsDecoded(t *testing.T) {
	dc := mustDaoConfig(t, &Config{DatabaseURL: "mysql://cube:p%23a%3Fb%2Fc@10.0.0.5:3306/mydb"})
	if dc.Pwd != "p#a?b/c" {
		t.Errorf("Pwd = %q, want p#a?b/c", dc.Pwd)
	}
	if dc.User != "cube" || dc.Addr != "10.0.0.5:3306" || dc.DBName != "mydb" {
		t.Errorf("User/Addr/DBName = %q/%q/%q, want cube/10.0.0.5:3306/mydb", dc.User, dc.Addr, dc.DBName)
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
		"mysql://user:secret#frag@10.0.0.1:3306/db", // rejected by url.Parse
		"mysql://user:secret@10.0.0.1:badport/db",   // rejected by url.Parse
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
// URL-reserved characters must round-trip intact.
func TestLoad_MysqlFieldsFromEnv(t *testing.T) {
	// Clear any ambient DB env vars so the split-field path is actually tested.
	for _, k := range []string{
		"DATABASE_URL",
		"CUBE_SANDBOX_MYSQL_HOST", "CUBE_SANDBOX_MYSQL_PORT",
		"CUBE_SANDBOX_MYSQL_USER", "CUBE_SANDBOX_MYSQL_PASSWORD", "CUBE_SANDBOX_MYSQL_DB",
	} {
		t.Setenv(k, "")
	}
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
// mysql_* keys. URL-reserved characters in the password must round-trip
// intact.
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

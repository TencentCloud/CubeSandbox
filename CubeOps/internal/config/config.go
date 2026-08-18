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
	Bind        string `yaml:"bind"`
	LogLevel    string `yaml:"log_level"`
	LogDir      string `yaml:"log_dir"`
	LogFileNum  int    `yaml:"log_file_num"`
	LogFileSize int    `yaml:"log_file_size"`
	JWTSecret   string `yaml:"jwt_secret"`

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

	// SoftDeletePurge (issue #973) configures the scheduled hard-purge of
	// soft-deleted (tombstoned) rows. All fields optional; defaults are enforced
	// by CubeDB/tombstone (7-day retention, hourly). DISABLED by default — the
	// purge is irreversible, so it must be opted into explicitly.
	SoftDeletePurge SoftDeletePurgeConf `yaml:"soft_delete_purge"`
	// Webhook delivery worker. Disabled by default; when enabled CubeOps
	// consumes the lifecycle stream and delivers sandbox events to
	// subscribed endpoints (see docs/zh/guide/webhook.md).
	Webhook WebhookConfig `yaml:"webhook"`
}

// SoftDeletePurgeConf configures the CubeOps tombstone purger.
type SoftDeletePurgeConf struct {
	Enable    *bool         `yaml:"enable"` // nil -> default-off (irreversible; opt in)
	DryRun    bool          `yaml:"dry_run"`
	Retention time.Duration `yaml:"retention"` // <=0 -> 7d; (0,1h) clamped up to 1h
	Interval  time.Duration `yaml:"interval"`  // <=0 -> 1h; (0,1m) clamped up to 1m
}

// WebhookConfig controls the CubeOps in-process webhook delivery worker.
// Every field has a CUBE_OPS_WEBHOOK_* environment override and a default
// applied in Load(); see config.example.yaml for the annotated section.
type WebhookConfig struct {
	// Enabled turns the worker (consumer + sender) on. Defaults to false so
	// existing deployments are unaffected until they opt in.
	Enabled bool `yaml:"enabled"`
	// ConsumerGroup is the Redis Stream consumer group name. Must not equal
	// CLM's "cube-proxy-sidecar" group.
	ConsumerGroup string `yaml:"consumer_group"`
	// ConsumerName identifies this process in the group. Empty → auto
	// <hostname>-<random suffix>; must be unique per process instance.
	ConsumerName string `yaml:"consumer_name"`
	// ReadBlock is the blocking XREADGROUP timeout.
	ReadBlock time.Duration `yaml:"read_block"`
	// ConsumerBatchSize is both the XREADGROUP COUNT and the per-cycle
	// materialization cap (read exactly what is materialized).
	ConsumerBatchSize int `yaml:"consumer_batch_size"`
	// RetryPollInterval is the slow poll used when no tasks were claimed.
	RetryPollInterval time.Duration `yaml:"retry_poll_interval"`
	// ClaimBatchSize is the per-round delivery claim cap (>= worker_concurrency).
	ClaimBatchSize int `yaml:"claim_batch_size"`
	// BacklogWatermark is the global actionable backlog (pending + retryable
	// failed, excluding window-expired rows) that pauses consumer reads.
	BacklogWatermark int `yaml:"backlog_watermark"`
	// PerSubscriptionBacklogLimit soft-limits sending per subscription.
	PerSubscriptionBacklogLimit int `yaml:"per_subscription_backlog_limit"`
	// KeepPendingMaxRetryWindow bounds keep-pending retries; expired rows stop
	// being claimed and are swept to dead. 0 = infinite (alerting + SOP).
	KeepPendingMaxRetryWindow time.Duration `yaml:"keep_pending_max_retry_window"`
	// MaxSubscriptionsPerEvent is the materialization chunk size.
	MaxSubscriptionsPerEvent int `yaml:"max_subscriptions_per_event"`
	// LeaseDuration is the user-facing claim lease floor; the effective lease
	// is max(LeaseDuration, 2*HTTPTimeout).
	LeaseDuration time.Duration `yaml:"lease_duration"`
	// HTTPTimeout bounds each delivery request.
	HTTPTimeout time.Duration `yaml:"http_timeout"`
	// ShutdownTimeout bounds graceful shutdown's wait for in-flight sends
	// (they are not cancelled during this window).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// MaxAttempts caps recorded send failures in dead-letter mode.
	MaxAttempts int `yaml:"max_attempts"`
	// WorkerConcurrency is the total in-flight HTTP pool size.
	WorkerConcurrency int `yaml:"worker_concurrency"`
	// PerSubscriptionConcurrency caps in-flight sends per subscription
	// (per-replica semantics; total = replicas × value).
	PerSubscriptionConcurrency int `yaml:"per_subscription_concurrency"`
	// DeadLetterMode: "keep-pending" (default) or "dead-letter".
	DeadLetterMode string `yaml:"dead_letter_mode"`
	// AllowPrivateNetworks bypasses SSRF rejection of RFC1918 addresses.
	AllowPrivateNetworks bool `yaml:"allow_private_networks"`
	// Cleanup configures terminal-row retention.
	Cleanup WebhookCleanupConfig `yaml:"cleanup"`

	// keepPendingZero records an explicit 0 for KeepPendingMaxRetryWindow
	// (infinite retries). Zero and unset are otherwise indistinguishable on
	// time.Duration; the env override sets this when the value is "0".
	keepPendingZero bool
}

// WebhookCleanupConfig controls retention cleanup of terminal delivery rows.
type WebhookCleanupConfig struct {
	// SucceededRetention keeps succeeded rows (default 720h = 30 days).
	SucceededRetention time.Duration `yaml:"succeeded_retention"`
	// TerminalFailureRetention keeps permanent_failed/dead/materialization
	// failure rows (default 2160h = 90 days). Retryable failed rows are never
	// cleaned by retention.
	TerminalFailureRetention time.Duration `yaml:"terminal_failure_retention"`
	// Interval is the cleanup / keep-pending sweep period (default 24h).
	Interval time.Duration `yaml:"interval"`
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
	if cfg.LogDir == "" {
		cfg.LogDir = "/data/log/CubeOps"
	}
	if cfg.LogFileNum == 0 {
		cfg.LogFileNum = 10
	}
	if cfg.LogFileSize == 0 {
		cfg.LogFileSize = 100
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
	applyWebhookDefaults(&cfg.Webhook)

	// JWT_SECRET is optional — if not set, it will be auto-generated and
	// persisted to the DB on first startup (see store.bootstrapJWTSecret).
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("database_url (or mysql_host + mysql_user + mysql_password + mysql_db) is required (set in YAML %s or via DATABASE_URL env)",
			yamlConfigPath())
	}
	if err := validateWebhookConfig(&cfg.Webhook, cfg.RedisURL); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyWebhookDefaults fills zero-valued webhook knobs with their defaults.
func applyWebhookDefaults(w *WebhookConfig) {
	if w.ConsumerGroup == "" {
		w.ConsumerGroup = "cube-webhook"
	}
	if w.ReadBlock == 0 {
		w.ReadBlock = 5 * time.Second
	}
	if w.ConsumerBatchSize == 0 {
		w.ConsumerBatchSize = 10
	}
	if w.RetryPollInterval == 0 {
		w.RetryPollInterval = time.Second
	}
	if w.ClaimBatchSize == 0 {
		w.ClaimBatchSize = 32
	}
	if w.BacklogWatermark == 0 {
		w.BacklogWatermark = 10000
	}
	if w.PerSubscriptionBacklogLimit == 0 {
		w.PerSubscriptionBacklogLimit = 1000
	}
	if w.KeepPendingMaxRetryWindow == 0 && !w.keepPendingZero {
		w.KeepPendingMaxRetryWindow = 168 * time.Hour
	}
	if w.MaxSubscriptionsPerEvent == 0 {
		w.MaxSubscriptionsPerEvent = 200
	}
	if w.LeaseDuration == 0 {
		w.LeaseDuration = 60 * time.Second
	}
	if w.HTTPTimeout == 0 {
		w.HTTPTimeout = 10 * time.Second
	}
	if w.ShutdownTimeout == 0 {
		w.ShutdownTimeout = 30 * time.Second
	}
	if w.MaxAttempts == 0 {
		w.MaxAttempts = 5
	}
	if w.WorkerConcurrency == 0 {
		w.WorkerConcurrency = 8
	}
	if w.PerSubscriptionConcurrency == 0 {
		w.PerSubscriptionConcurrency = 2
	}
	if w.DeadLetterMode == "" {
		w.DeadLetterMode = "keep-pending"
	}
	if w.Cleanup.SucceededRetention == 0 {
		w.Cleanup.SucceededRetention = 720 * time.Hour
	}
	if w.Cleanup.TerminalFailureRetention == 0 {
		w.Cleanup.TerminalFailureRetention = 2160 * time.Hour
	}
	if w.Cleanup.Interval == 0 {
		w.Cleanup.Interval = 24 * time.Hour
	}
}

// validateWebhookConfig rejects unusable worker configurations when enabled.
func validateWebhookConfig(w *WebhookConfig, redisURL string) error {
	if !w.Enabled {
		return nil
	}
	if redisURL == "" {
		return fmt.Errorf("webhook.enabled requires redis_url (REDIS_URL)")
	}
	if w.ConsumerGroup == "cube-proxy-sidecar" {
		return fmt.Errorf("webhook.consumer_group must not be cube-proxy-sidecar (CLM owns that group)")
	}
	if w.WorkerConcurrency <= 0 {
		return fmt.Errorf("webhook.worker_concurrency must be > 0")
	}
	if w.ConsumerBatchSize <= 0 {
		return fmt.Errorf("webhook.consumer_batch_size must be > 0")
	}
	if w.ClaimBatchSize < w.WorkerConcurrency {
		return fmt.Errorf("webhook.claim_batch_size (%d) must be >= worker_concurrency (%d)",
			w.ClaimBatchSize, w.WorkerConcurrency)
	}
	effectiveLease := w.LeaseDuration
	if twice := 2 * w.HTTPTimeout; twice > effectiveLease {
		effectiveLease = twice
	}
	if w.HTTPTimeout >= effectiveLease {
		return fmt.Errorf("webhook.http_timeout must be < effective lease (max(lease_duration, 2*http_timeout))")
	}
	if w.DeadLetterMode != "keep-pending" && w.DeadLetterMode != "dead-letter" {
		return fmt.Errorf("webhook.dead_letter_mode must be keep-pending or dead-letter, got %q", w.DeadLetterMode)
	}
	return nil
}

// DaoConfig converts the CubeOps config to a CubeDB dao.Config.
//
// If DatabaseURL is set, it is the single source of truth and the individual
// MySQL* fields are ignored (accepting a DatabaseURL while silently
// connecting from the individual fields has historically caused empty
// user/db connections). The driver is selected from the URL scheme
// (mysql:// or postgres://) so PostgreSQL deployments work instead of
// silently falling back to MySQL and failing on dialect-specific SQL.
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
	if v := os.Getenv("CUBE_OPS_LOG_DIR"); v != "" {
		cfg.LogDir = v
	}
	if v := os.Getenv("CUBE_OPS_LOG_FILE_NUM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LogFileNum = n
		}
	}
	if v := os.Getenv("CUBE_OPS_LOG_FILE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LogFileSize = n
		}
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
	overrideWebhookFromEnv(&cfg.Webhook)
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

// overrideWebhookFromEnv applies CUBE_OPS_WEBHOOK_* environment overrides.
func overrideWebhookFromEnv(w *WebhookConfig) {
	w.Enabled = envBool("CUBE_OPS_WEBHOOK_ENABLED", w.Enabled)
	if v := os.Getenv("CUBE_OPS_WEBHOOK_CONSUMER_GROUP"); v != "" {
		w.ConsumerGroup = v
	}
	if v := os.Getenv("CUBE_OPS_WEBHOOK_CONSUMER_NAME"); v != "" {
		w.ConsumerName = v
	}
	w.ReadBlock = envDuration("CUBE_OPS_WEBHOOK_READ_BLOCK", w.ReadBlock)
	w.ConsumerBatchSize = envInt("CUBE_OPS_WEBHOOK_CONSUMER_BATCH_SIZE", w.ConsumerBatchSize)
	w.RetryPollInterval = envDuration("CUBE_OPS_WEBHOOK_RETRY_POLL_INTERVAL", w.RetryPollInterval)
	w.ClaimBatchSize = envInt("CUBE_OPS_WEBHOOK_CLAIM_BATCH_SIZE", w.ClaimBatchSize)
	w.BacklogWatermark = envInt("CUBE_OPS_WEBHOOK_BACKLOG_WATERMARK", w.BacklogWatermark)
	w.PerSubscriptionBacklogLimit = envInt("CUBE_OPS_WEBHOOK_PER_SUBSCRIPTION_BACKLOG_LIMIT", w.PerSubscriptionBacklogLimit)
	if v := os.Getenv("CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW"); v != "" {
		if v == "0" {
			w.KeepPendingMaxRetryWindow = 0
			w.keepPendingZero = true
		} else if d, err := time.ParseDuration(v); err == nil {
			w.KeepPendingMaxRetryWindow = d
		}
	}
	w.MaxSubscriptionsPerEvent = envInt("CUBE_OPS_WEBHOOK_MAX_SUBSCRIPTIONS_PER_EVENT", w.MaxSubscriptionsPerEvent)
	w.LeaseDuration = envDuration("CUBE_OPS_WEBHOOK_LEASE_DURATION", w.LeaseDuration)
	w.HTTPTimeout = envDuration("CUBE_OPS_WEBHOOK_HTTP_TIMEOUT", w.HTTPTimeout)
	w.ShutdownTimeout = envDuration("CUBE_OPS_WEBHOOK_SHUTDOWN_TIMEOUT", w.ShutdownTimeout)
	w.MaxAttempts = envInt("CUBE_OPS_WEBHOOK_MAX_ATTEMPTS", w.MaxAttempts)
	w.WorkerConcurrency = envInt("CUBE_OPS_WEBHOOK_WORKER_CONCURRENCY", w.WorkerConcurrency)
	w.PerSubscriptionConcurrency = envInt("CUBE_OPS_WEBHOOK_PER_SUBSCRIPTION_CONCURRENCY", w.PerSubscriptionConcurrency)
	if v := os.Getenv("CUBE_OPS_WEBHOOK_DEAD_LETTER_MODE"); v != "" {
		w.DeadLetterMode = v
	}
	w.AllowPrivateNetworks = envBool("CUBE_OPS_WEBHOOK_ALLOW_PRIVATE_NETWORKS", w.AllowPrivateNetworks)
	w.Cleanup.SucceededRetention = envDuration("CUBE_OPS_WEBHOOK_CLEANUP_SUCCEEDED_RETENTION", w.Cleanup.SucceededRetention)
	w.Cleanup.TerminalFailureRetention = envDuration("CUBE_OPS_WEBHOOK_CLEANUP_TERMINAL_FAILURE_RETENTION", w.Cleanup.TerminalFailureRetention)
	w.Cleanup.Interval = envDuration("CUBE_OPS_WEBHOOK_CLEANUP_INTERVAL", w.Cleanup.Interval)
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return strings.EqualFold(v, "true") || v == "1"
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return fallback
}

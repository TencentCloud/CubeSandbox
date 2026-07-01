// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the cube-lifecycle-manager's runtime parameters. The shape is
// intentionally flat — every field maps to a single env var — so the operator
// can wire it up via systemd EnvironmentFile= without a YAML parser dependency.
type Config struct {
	// Redis (the same instance CubeMaster writes to).
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// CubeProxy admin endpoints to push to and pull from. Multiple endpoints
	// are supported even though the recommended deployment is one sidecar
	// per CubeProxy: future operators may consolidate.
	CubeProxyAdminURLs []string
	CubeAdminToken     string // optional shared secret; sent as X-Cube-Admin-Token

	// CubeMaster internal HTTP for pause/resume. Sidecar calls
	// POST <CubeMasterURL>/cube/sandbox/update with action=pause|resume.
	CubeMasterURL string

	// HTTP listener for /internal/resume (called by CubeProxy via the
	// internal sub-location). In the standalone deployment CLM is reached
	// across the intra-cluster network, so the default binds to 0.0.0.0;
	// override to a loopback address for single-host dev setups.
	ListenAddr string

	// Defaults applied when a sandbox's lifecycle meta omits TimeoutSeconds.
	DefaultIdleTimeout time.Duration

	// Loop intervals.
	StreamReadBlock  time.Duration // XREADGROUP BLOCK arg
	LastActivePoll   time.Duration // GET /admin/last_active cadence
	IdleSweepInterval time.Duration // sweeper cadence
	// BootstrapWarmup: after sidecar restart, wait this long before pausing
	// any sandbox that was loaded via HGETALL bootstrap. Lets the
	// last_active poller backfill activity timestamps first. New sandboxes
	// that arrive AFTER startup are not affected by this delay.
	BootstrapWarmup time.Duration

	// Pause/resume locks (SETNX TTL). Long enough to outlive a slow
	// CubeMaster RPC, short enough that a crashed sidecar releases the lock.
	StateLockTTL time.Duration

	// Consumer group identity. Group name is fixed; consumer name defaults
	// to the host's name so multiple sidecars in a cluster get independent
	// pending-entries lists.
	ConsumerGroup string
	ConsumerName  string // empty → derived from os.Hostname()

	// HTTP client timeouts (for outbound calls to CubeMaster + CubeProxy).
	HTTPTimeout time.Duration

	// UseStaticFleet, when true, disables Redis-based service discovery and
	// treats CubeProxyAdminURLs as the authoritative fleet. Set via env var
	// CUBE_LCM_USE_STATIC_FLEET=1 for single-host dev / integration tests.
	UseStaticFleet bool

	// Discovery loop tuning.
	// HeartbeatTTL: a CubeProxy whose last heartbeat is older than this is
	// treated as offline. Should be a small multiple of the proxy's own
	// heartbeat interval (default sizing: 3 × 5s = 15s).
	HeartbeatTTL time.Duration
	// DiscoveryRefresh: cadence of the Redis heartbeat scan.
	DiscoveryRefresh time.Duration
}

// Default returns a config populated with safe defaults; callers then override
// via Load(env) or direct field writes (tests).
func Default() *Config {
	return &Config{
		RedisAddr:          "127.0.0.1:6379",
		RedisDB:            0,
		CubeProxyAdminURLs: []string{"http://127.0.0.1:8082"},
		// CubeMaster's HTTP listener defaults to :8089 (config key
		// `common.http_port`). Override via CUBE_LCM_CUBEMASTER_URL.
		CubeMasterURL:      "http://127.0.0.1:8089",
		ListenAddr:         "0.0.0.0:8083",
		DefaultIdleTimeout: 5 * time.Minute,
		StreamReadBlock:    5 * time.Second,
		LastActivePoll:     5 * time.Second,
		IdleSweepInterval:  5 * time.Second,
		BootstrapWarmup:    30 * time.Second,
		StateLockTTL:       60 * time.Second,
		// ConsumerGroup name is intentionally the legacy "cube-proxy-sidecar"
		// value so an in-place upgrade (old sidecar -> CLM) keeps consuming
		// from the same pending-entries list without reprocessing history.
		ConsumerGroup:    "cube-proxy-sidecar",
		HTTPTimeout:      10 * time.Second,
		UseStaticFleet:   false,
		HeartbeatTTL:     15 * time.Second,
		DiscoveryRefresh: 3 * time.Second,
	}
}

// Load builds a Config from environment variables, falling back to Default()
// for unset fields. Returns an error only when an env var is set to a value
// that cannot be parsed — missing values are not an error.
func Load() (*Config, error) {
	c := Default()

	var errs []string
	addErr := func(name string, err error) {
		errs = append(errs, fmt.Sprintf("%s: %v", name, err))
	}

	if v := os.Getenv("CUBE_LCM_REDIS_ADDR"); v != "" {
		c.RedisAddr = v
	}
	if v := os.Getenv("CUBE_LCM_REDIS_PASSWORD"); v != "" {
		c.RedisPassword = v
	}
	if v := os.Getenv("CUBE_LCM_REDIS_DB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			addErr("CUBE_LCM_REDIS_DB", err)
		} else {
			c.RedisDB = n
		}
	}
	if v := os.Getenv("CUBE_LCM_PROXY_ADMIN_URLS"); v != "" {
		c.CubeProxyAdminURLs = splitAndTrim(v)
	}
	if v := os.Getenv("CUBE_LCM_ADMIN_TOKEN"); v != "" {
		c.CubeAdminToken = v
	}
	if v := os.Getenv("CUBE_LCM_CUBEMASTER_URL"); v != "" {
		c.CubeMasterURL = v
	}
	if v := os.Getenv("CUBE_LCM_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("CUBE_LCM_DEFAULT_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_DEFAULT_IDLE_TIMEOUT", err)
		} else {
			c.DefaultIdleTimeout = d
		}
	}
	if v := os.Getenv("CUBE_LCM_LAST_ACTIVE_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_LAST_ACTIVE_POLL", err)
		} else {
			c.LastActivePoll = d
		}
	}
	if v := os.Getenv("CUBE_LCM_IDLE_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_IDLE_SWEEP_INTERVAL", err)
		} else {
			c.IdleSweepInterval = d
		}
	}
	if v := os.Getenv("CUBE_LCM_BOOTSTRAP_WARMUP"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_BOOTSTRAP_WARMUP", err)
		} else {
			c.BootstrapWarmup = d
		}
	}
	if v := os.Getenv("CUBE_LCM_STATE_LOCK_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_STATE_LOCK_TTL", err)
		} else {
			c.StateLockTTL = d
		}
	}
	if v := os.Getenv("CUBE_LCM_CONSUMER_NAME"); v != "" {
		c.ConsumerName = v
	}
	if v := os.Getenv("CUBE_LCM_USE_STATIC_FLEET"); v != "" {
		// Accept the usual truthy set; anything else is treated as false.
		switch v {
		case "1", "true", "TRUE", "yes":
			c.UseStaticFleet = true
		default:
			c.UseStaticFleet = false
		}
	}
	if v := os.Getenv("CUBE_LCM_HEARTBEAT_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_HEARTBEAT_TTL", err)
		} else {
			c.HeartbeatTTL = d
		}
	}
	if v := os.Getenv("CUBE_LCM_DISCOVERY_REFRESH"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			addErr("CUBE_LCM_DISCOVERY_REFRESH", err)
		} else {
			c.DiscoveryRefresh = d
		}
	}

	if c.ConsumerName == "" {
		host, err := os.Hostname()
		if err != nil {
			addErr("hostname", err)
		} else {
			c.ConsumerName = host
		}
	}

	if len(errs) > 0 {
		return nil, errors.New("config load: " + strings.Join(errs, "; "))
	}
	return c, nil
}

// Validate returns an error if the config has any field combination that the
// sidecar can't proceed with.
func (c *Config) Validate() error {
	if c.RedisAddr == "" {
		return errors.New("redis addr is empty")
	}
	if len(c.CubeProxyAdminURLs) == 0 {
		return errors.New("cube proxy admin urls is empty")
	}
	if c.CubeMasterURL == "" {
		return errors.New("cube master url is empty")
	}
	if c.ListenAddr == "" {
		return errors.New("listen addr is empty")
	}
	if c.ConsumerName == "" {
		return errors.New("consumer name is empty")
	}
	if c.IdleSweepInterval <= 0 {
		return errors.New("idle sweep interval must be > 0")
	}
	if c.LastActivePoll <= 0 {
		return errors.New("last active poll must be > 0")
	}
	return nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

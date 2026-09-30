// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisclient

import (
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
)

// tlsConfig returns the TLS settings for the Redis connection, or nil when
// plaintext is configured. Managed Redis with in-transit encryption refuses
// plaintext, so this has to be requested explicitly.
func tlsConfig(cfg *config.Config) *tls.Config {
	if !cfg.RedisTLS {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// New builds a go-redis client for standalone or Sentinel mode.
func New(cfg *config.Config) redis.UniversalClient {
	if cfg.RedisMasterName != "" {
		// Do not fall back to RedisPassword: many deployments only set
		// requirepass on the Redis master, while Sentinel has no AUTH.
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.RedisMasterName,
			SentinelAddrs:    cfg.RedisSentinelAddrs,
			Password:         cfg.RedisPassword,
			SentinelPassword: cfg.RedisSentinelPassword,
			DB:               cfg.RedisDB,
			TLSConfig:        tlsConfig(cfg),
		})
	}
	return redis.NewClient(&redis.Options{
		Addr:      cfg.RedisAddr,
		Password:  cfg.RedisPassword,
		DB:        cfg.RedisDB,
		TLSConfig: tlsConfig(cfg),
	})
}

// DisplayAddr returns a log-friendly Redis endpoint description.
func DisplayAddr(cfg *config.Config) string {
	if cfg.RedisMasterName != "" {
		return fmt.Sprintf("sentinel:%s(%s)", cfg.RedisMasterName, strings.Join(cfg.RedisSentinelAddrs, ","))
	}
	return cfg.RedisAddr
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package redisclient

import (
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
)

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
		})
	}
	return redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
}

// DisplayAddr returns a log-friendly Redis endpoint description.
func DisplayAddr(cfg *config.Config) string {
	if cfg.RedisMasterName != "" {
		return fmt.Sprintf("sentinel:%s(%s)", cfg.RedisMasterName, strings.Join(cfg.RedisSentinelAddrs, ","))
	}
	return cfg.RedisAddr
}

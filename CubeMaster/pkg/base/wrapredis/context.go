// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package wrapredis

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// DialContext resolves a fresh master and authenticates/selects under the same
// caller deadline. It does not change RedisWrap's legacy retries or timeouts.
// A deadline is required; cancellation closes actual sockets through redigo.
func DialContext(ctx context.Context, conf config.RedisConf) (redis.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, fmt.Errorf("redis dial requires a deadline")
	}
	dial := func(addr, password string, db int) (redis.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		return redis.DialContext(ctx, "tcp", addr, redis.DialConnectTimeout(remaining),
			redis.DialReadTimeout(remaining), redis.DialWriteTimeout(remaining),
			redis.DialPassword(password), redis.DialDatabase(db))
	}
	var addr string
	if conf.MasterName == "" {
		var err error
		addr, err = resolveRedisAddr(&conf)
		if err != nil {
			return nil, err
		}
	} else {
		sentinels := parseRedisAddrs(conf.SentinelNodes)
		if len(sentinels) == 0 {
			return nil, fmt.Errorf("sentinel_nodes is required")
		}
		var lastErr error
		for _, endpoint := range sentinels {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			c, err := dial(endpoint, conf.SentinelPassword, 0)
			if err != nil {
				lastErr = err
				continue
			}
			reply, err := redis.Strings(redis.DoContext(c, ctx, "SENTINEL", "get-master-addr-by-name", conf.MasterName))
			c.Close()
			if err != nil {
				lastErr = err
				continue
			}
			if len(reply) != 2 {
				lastErr = fmt.Errorf("invalid sentinel master address")
				continue
			}
			addr = net.JoinHostPort(reply[0], reply[1])
			break
		}
		if addr == "" {
			return nil, fmt.Errorf("sentinel lookup failed: %w", lastErr)
		}
	}
	return dial(addr, conf.Password, conf.DbNo)
}

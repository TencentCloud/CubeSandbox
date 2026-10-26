// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package wrapredis

import (
	"fmt"
	"net"
	"strings"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// resolveRedisAddr returns the TCP endpoint to dial for Redis commands.
// Standalone mode uses Nodes directly; sentinel mode queries SENTINEL
// get-master-addr-by-name against SentinelNodes.
func resolveRedisAddr(conf *config.RedisConf) (string, error) {
	if conf == nil {
		return "", fmt.Errorf("redis conf is nil")
	}
	if conf.MasterName == "" {
		if strings.TrimSpace(conf.Nodes) == "" {
			return "", fmt.Errorf("redis nodes is empty")
		}
		return strings.TrimSpace(conf.Nodes), nil
	}
	return lookupSentinelMaster(conf)
}

// redisDisplayAddr returns a human-readable address for logs/metrics.
func redisDisplayAddr(conf *config.RedisConf) string {
	if conf == nil {
		return ""
	}
	if conf.MasterName == "" {
		return conf.Nodes
	}
	return fmt.Sprintf("sentinel:%s(%s)", conf.MasterName, conf.SentinelNodes)
}

func lookupSentinelMaster(conf *config.RedisConf) (string, error) {
	sentinels := parseRedisAddrs(conf.SentinelNodes)
	if len(sentinels) == 0 {
		return "", fmt.Errorf("sentinel_nodes is required when master_name is set")
	}
	// Do not fall back to Password: many deployments only set requirepass on
	// the Redis master, while Sentinel has no AUTH configured.
	sentinelPwd := conf.SentinelPassword
	var lastErr error
	for _, sentinelAddr := range sentinels {
		addr, err := sentinelGetMaster(sentinelAddr, conf.MasterName, sentinelPwd)
		if err == nil {
			return addr, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("sentinel lookup for master %q failed: %w", conf.MasterName, lastErr)
}

func sentinelGetMaster(sentinelAddr, masterName, password string) (string, error) {
	c, err := redis.Dial("tcp", sentinelAddr,
		redis.DialConnectTimeout(dialTimeout),
		redis.DialReadTimeout(dialTimeout),
		redis.DialWriteTimeout(dialTimeout),
	)
	if err != nil {
		return "", fmt.Errorf("dial sentinel %s: %w", sentinelAddr, err)
	}
	defer c.Close()

	if password != "" {
		// redigo returns AUTH failures as redis.Error reply with err==nil;
		// redis.String maps that to a real error so wrong passwords fail here.
		if _, err := redis.String(c.Do("AUTH", password)); err != nil {
			return "", fmt.Errorf("auth sentinel %s: %w", sentinelAddr, err)
		}
	}

	reply, err := redis.Strings(c.Do("SENTINEL", "get-master-addr-by-name", masterName))
	if err != nil {
		return "", fmt.Errorf("sentinel get-master-addr-by-name %q via %s: %w", masterName, sentinelAddr, err)
	}
	if len(reply) != 2 {
		return "", fmt.Errorf("sentinel %s returned unexpected master addr for %q: %v", sentinelAddr, masterName, reply)
	}
	return net.JoinHostPort(reply[0], reply[1]), nil
}

func parseRedisAddrs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Align with CubeProxy split_host_port and one-click preflight:
		// bare host / bare [ipv6] defaults to Sentinel port 26379.
		// host:port and [ipv6]:port are left unchanged for redis.Dial.
		if !strings.Contains(part, ":") {
			part = net.JoinHostPort(part, "26379")
		} else if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
			host := strings.TrimSuffix(strings.TrimPrefix(part, "["), "]")
			part = net.JoinHostPort(host, "26379")
		}
		out = append(out, part)
	}
	return out
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
)

const (
	nodeMetricKeyPrefix       = "cube:v1:master:node:metric:"
	legacyNodeMetricKeyPrefix = "cube:v1:redis_node_info:"
	nodeMetricTTLSec          = 600
	redisDialTimeout          = 3 * time.Second
)

var (
	pool     *redis.Pool
	poolOnce sync.Once

	// writeNodeMetricHook is set by tests to intercept Redis writes.
	writeNodeMetricHook func(*NodeMetric) error
)

// SetWriteNodeMetricHook registers a hook for tests. The returned cleanup
// restores the previous hook.
func SetWriteNodeMetricHook(hook func(*NodeMetric) error) func() {
	prev := writeNodeMetricHook
	writeNodeMetricHook = hook
	return func() { writeNodeMetricHook = prev }
}

// Init initializes the Redis pool. No-op when Redis is not configured.
// Supports standalone (REDIS_URL/HOST) and Sentinel (MASTER_NAME) modes.
func Init(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	sentinel := cfg.RedisMasterName != ""
	url := cfg.RedisURL
	if !sentinel && url == "" && cfg.RedisHost != "" {
		url = buildRedisURL(cfg.RedisHost, cfg.RedisPort, cfg.RedisPassword)
	}
	if !sentinel && url == "" {
		return nil
	}
	poolOnce.Do(func() {
		pool = &redis.Pool{
			MaxIdle:     5,
			MaxActive:   20,
			IdleTimeout: 5 * time.Minute,
			Dial: func() (redis.Conn, error) {
				if sentinel {
					return dialSentinel(cfg)
				}
				return redis.DialURL(url,
					redis.DialConnectTimeout(redisDialTimeout),
					redis.DialReadTimeout(redisDialTimeout),
					redis.DialWriteTimeout(redisDialTimeout),
				)
			},
		}
	})
	conn := pool.Get()
	defer conn.Close()
	if _, err := conn.Do("PING"); err != nil {
		logging.G(context.Background()).Errorf("nodemetric: redis ping failed: %v", err)
		return fmt.Errorf("nodemetric: redis ping failed: %w", err)
	}
	logging.G(context.Background()).Infof("nodemetric: redis pool initialized")
	return nil
}

// dialSentinel resolves the current master via SENTINEL get-master-addr-by-name,
// then dials it directly.
func dialSentinel(cfg *config.Config) (redis.Conn, error) {
	addr, err := lookupSentinelMaster(cfg.RedisSentinelNodes, cfg.RedisMasterName, cfg.RedisSentinelPassword)
	if err != nil {
		return nil, err
	}
	return redis.Dial("tcp", addr,
		redis.DialConnectTimeout(redisDialTimeout),
		redis.DialReadTimeout(redisDialTimeout),
		redis.DialWriteTimeout(redisDialTimeout),
		redis.DialPassword(cfg.RedisPassword),
	)
}

// lookupSentinelMaster queries each sentinel for the master address.
func lookupSentinelMaster(sentinelNodes, masterName, sentinelPwd string) (string, error) {
	sentinels := parseRedisAddrs(sentinelNodes)
	if len(sentinels) == 0 {
		return "", fmt.Errorf("sentinel_nodes is required when master_name is set")
	}
	var lastErr error
	for _, s := range sentinels {
		c, err := redis.Dial("tcp", s,
			redis.DialConnectTimeout(redisDialTimeout),
			redis.DialReadTimeout(redisDialTimeout),
			redis.DialWriteTimeout(redisDialTimeout),
		)
		if err != nil {
			lastErr = fmt.Errorf("dial sentinel %s: %w", s, err)
			continue
		}
		if sentinelPwd != "" {
			if _, err := redis.String(c.Do("AUTH", sentinelPwd)); err != nil {
				c.Close()
				lastErr = fmt.Errorf("auth sentinel %s: %w", s, err)
				continue
			}
		}
		reply, err := redis.Strings(c.Do("SENTINEL", "get-master-addr-by-name", masterName))
		c.Close()
		if err != nil {
			lastErr = fmt.Errorf("sentinel get-master-addr-by-name %q via %s: %w", masterName, s, err)
			continue
		}
		if len(reply) != 2 {
			lastErr = fmt.Errorf("sentinel %s returned unexpected reply for %q: %v", s, masterName, reply)
			continue
		}
		return net.JoinHostPort(reply[0], reply[1]), nil
	}
	return "", fmt.Errorf("sentinel lookup for master %q failed: %w", masterName, lastErr)
}

// parseRedisAddrs splits comma-separated host:port pairs; bare hosts default
// to sentinel port 26379.
func parseRedisAddrs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, ":") {
			p = net.JoinHostPort(p, "26379")
		}
		out = append(out, p)
	}
	return out
}

// buildRedisURL assembles a redis:// URL from split host/port/password.
func buildRedisURL(host string, port int, password string) string {
	if port == 0 {
		port = 6379
	}
	u := &url.URL{
		Scheme: "redis",
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   "/0",
	}
	if password != "" {
		u.User = url.UserPassword("", password)
	}
	return u.String()
}

// NodeMetric is the per-node resource snapshot written to Redis.
// Field names mirror CubeMaster's localcache.NodeMetric / RedisNodeInfo.
type NodeMetric struct {
	NodeID     string
	MetricTime time.Time

	HasAllocated  bool
	MilliCPUUsage int64
	MemoryMBUsage int64
	MvmNum        int64
	NicQueues     int64

	HasDisk             bool
	DataDiskUsagePer    float64
	StorageDiskUsagePer float64
	SysDiskUsagePer     float64
}

// WriteNodeMetric writes the metric snapshot to Redis, mirroring
// CubeMaster's localcache.WriteNodeMetric. Only the groups the cubelet
// actually reported are written (HasAllocated / HasDisk).
func WriteNodeMetric(m *NodeMetric) error {
	if m == nil || m.NodeID == "" {
		return errors.New("WriteNodeMetric: node id required")
	}
	if !m.HasAllocated && !m.HasDisk {
		return nil
	}
	if m.MetricTime.IsZero() {
		m.MetricTime = time.Now()
	}
	if writeNodeMetricHook != nil {
		return writeNodeMetricHook(m)
	}
	if pool == nil {
		return nil
	}
	updateAt, err := m.MetricTime.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal metric time: %w", err)
	}
	key := nodeMetricKeyPrefix + m.NodeID
	fields := []any{
		"ins_id", m.NodeID,
		"update_at", string(updateAt),
	}
	if m.HasAllocated {
		fields = append(fields,
			"quota_cpu_usage", m.MilliCPUUsage,
			"quota_mem_mb_usage", m.MemoryMBUsage,
			"mvm_num", m.MvmNum,
			"nic_queues", m.NicQueues,
		)
	}
	if m.HasDisk {
		fields = append(fields,
			"data_disk_usage_per", m.DataDiskUsagePer,
			"storage_disk_usage_per", m.StorageDiskUsagePer,
			"sys_disk_usage_per", m.SysDiskUsagePer,
		)
	}
	conn := pool.Get()
	defer conn.Close()
	if _, err := conn.Do("HSET", redis.Args{key}.Add(fields...)...); err != nil {
		logging.G(context.Background()).Warnf("nodemetric: redis HSET failed: node=%s: %v", m.NodeID, err)
		return err
	}
	if nodeMetricTTLSec > 0 {
		_, _ = conn.Do("EXPIRE", key, nodeMetricTTLSec)
	}
	return nil
}

// deleteNodeMetricHook is set by tests to intercept Redis deletes.
var deleteNodeMetricHook func(nodeID string) error

// SetDeleteNodeMetricHook registers a hook for tests. The returned
// cleanup restores the previous hook.
func SetDeleteNodeMetricHook(hook func(nodeID string) error) func() {
	prev := deleteNodeMetricHook
	deleteNodeMetricHook = hook
	return func() { deleteNodeMetricHook = prev }
}

// DeleteNodeMetric removes the per-node metric hash (current and legacy
// CubeMaster key) from Redis; nil when Redis is not configured.
func DeleteNodeMetric(nodeID string) error {
	if nodeID == "" {
		return errors.New("DeleteNodeMetric: node id required")
	}
	if deleteNodeMetricHook != nil {
		return deleteNodeMetricHook(nodeID)
	}
	if pool == nil {
		return nil
	}
	keys := []string{
		nodeMetricKeyPrefix + nodeID,
		legacyNodeMetricKeyPrefix + nodeID,
	}
	args := redis.Args{}
	for _, k := range keys {
		args = args.Add(k)
	}
	conn := pool.Get()
	defer conn.Close()
	if _, err := conn.Do("DEL", args...); err != nil {
		logging.G(context.Background()).Warnf("nodemetric: redis DEL failed node=%s: %v", nodeID, err)
		return err
	}
	return nil
}

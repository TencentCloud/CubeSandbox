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

// Init initializes the Redis pool and verifies connectivity. Redis is required.
// REDIS_URL takes precedence over Sentinel and split host settings.
func Init(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("nodemetric: config is nil; redis is required")
	}
	url, sentinel, err := resolveRedisConnection(cfg)
	if err != nil {
		return err
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

// resolveRedisConnection applies the connection source priority:
// REDIS_URL, then Sentinel, then split host settings.
func resolveRedisConnection(cfg *config.Config) (string, bool, error) {
	if cfg.RedisURL != "" {
		return cfg.RedisURL, false, nil
	}
	if cfg.RedisMasterName != "" {
		return "", true, nil
	}
	if cfg.RedisHost != "" {
		return buildRedisURL(cfg.RedisHost, cfg.RedisPort, cfg.RedisDB, cfg.RedisPassword), false, nil
	}
	return "", false, errors.New("nodemetric: redis is not configured (set REDIS_URL or REDIS_HOST/REDIS_MASTER_NAME)")
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
		redis.DialDatabase(cfg.RedisDB),
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

// buildRedisURL assembles a redis:// URL from split host/port/db/password.
func buildRedisURL(host string, port int, db int, password string) string {
	if port == 0 {
		port = 6379
	}
	u := &url.URL{
		Scheme: "redis",
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   fmt.Sprintf("/%d", db),
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

// readNodeMetricHook is set by tests to intercept Redis metric reads.
var readNodeMetricHook func(nodeID string) (*NodeMetric, error)

// SetReadNodeMetricHook registers a test hook; cleanup restores the prior one.
func SetReadNodeMetricHook(hook func(nodeID string) (*NodeMetric, error)) func() {
	prev := readNodeMetricHook
	readNodeMetricHook = hook
	return func() { readNodeMetricHook = prev }
}

// ReadNodeMetric reads the per-node metric hash from Redis. (nil, nil) on miss.
func ReadNodeMetric(nodeID string) (*NodeMetric, error) {
	if nodeID == "" {
		return nil, errors.New("ReadNodeMetric: node id required")
	}
	if readNodeMetricHook != nil {
		return readNodeMetricHook(nodeID)
	}
	if pool == nil {
		return nil, nil
	}
	conn := pool.Get()
	defer conn.Close()
	v, err := redis.Values(conn.Do("HGETALL", nodeMetricKeyPrefix+nodeID))
	if err != nil {
		return nil, fmt.Errorf("nodemetric: redis HGETALL failed: node=%s: %w", nodeID, err)
	}
	if len(v) == 0 {
		return nil, nil // miss
	}
	m, err := redis.StringMap(v, nil)
	if err != nil {
		return nil, fmt.Errorf("nodemetric: redis StringMap failed: node=%s: %w", nodeID, err)
	}
	nm := &NodeMetric{NodeID: nodeID}
	if raw, ok := m["update_at"]; ok {
		_ = nm.MetricTime.UnmarshalText([]byte(raw))
	}
	if _, ok := m["quota_cpu_usage"]; ok {
		nm.HasAllocated = true
		nm.MilliCPUUsage = atoi64(m["quota_cpu_usage"])
		nm.MemoryMBUsage = atoi64(m["quota_mem_mb_usage"])
		nm.MvmNum = atoi64(m["mvm_num"])
		nm.NicQueues = atoi64(m["nic_queues"])
	}
	if _, ok := m["data_disk_usage_per"]; ok {
		nm.HasDisk = true
		nm.DataDiskUsagePer = atof64(m["data_disk_usage_per"])
		nm.StorageDiskUsagePer = atof64(m["storage_disk_usage_per"])
		nm.SysDiskUsagePer = atof64(m["sys_disk_usage_per"])
	}
	return nm, nil
}

// atoi64 parses s as int64; 0 on error.
func atoi64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

// atof64 parses s as float64; 0 on error.
func atof64(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%g", &f)
	return f
}

// Ping checks Redis connectivity with a 1s timeout.
func Ping(ctx context.Context) error {
	if pool == nil {
		return errors.New("redis pool not initialized")
	}
	connCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c := pool.Get()
		defer c.Close()
		_, err := c.Do("PING")
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-connCtx.Done():
		return connCtx.Err()
	}
}

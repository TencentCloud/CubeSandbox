// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package wrapredis

import (
	"errors"
	"strings"
	"sync/atomic"

	"github.com/gomodule/redigo/redis"
)

var subscribeCursor int64

// subscribeCandidates lists the endpoints a Pub/Sub connection may dial. Any
// Cluster node serves ordinary (non-sharded) Pub/Sub for the whole cluster.
func (r *RedisWrap) subscribeCandidates() ([]string, error) {
	conf := r.redisConf
	if conf == nil {
		return nil, errors.New("redis conf is nil")
	}
	if conf.ClusterNodes != "" {
		addrs := parseRedisAddrs(conf.ClusterNodes)
		if len(addrs) == 0 {
			return nil, errors.New("cluster_nodes is empty")
		}
		// Rotate the starting seed so a dead node is not retried first forever.
		start := int(atomic.AddInt64(&subscribeCursor, 1)) % len(addrs)
		out := make([]string, 0, len(addrs))
		out = append(out, addrs[start:]...)
		return append(out, addrs[:start]...), nil
	}
	addr, err := resolveRedisAddr(conf)
	if err != nil {
		return nil, err
	}
	return []string{addr}, nil
}

// SubscribeConn returns a dedicated, unpooled connection for Pub/Sub. It has no
// default read timeout; the subscriber passes its own to ReceiveWithTimeout.
func (r *RedisWrap) SubscribeConn() (redis.Conn, error) {
	addrs, err := r.subscribeCandidates()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, addr := range addrs {
		if strings.TrimSpace(addr) == "" {
			continue
		}
		c, dialErr := redis.Dial("tcp", addr,
			redis.DialConnectTimeout(dialTimeout),
			redis.DialWriteTimeout(dialTimeout),
			redis.DialDatabase(r.redisConf.DbNo),
			redis.DialPassword(r.redisConf.Password))
		if dialErr == nil {
			return c, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("no reachable redis endpoint for subscribe")
	}
	return nil, lastErr
}

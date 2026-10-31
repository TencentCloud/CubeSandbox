// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package redisx provides Redis Cluster routing for existing redigo callers.
package redisx

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

type Cluster struct{ cluster *redisc.Cluster }

// NewCluster does not connect until the first command. Cluster always uses DB 0.
func NewCluster(nodes, password string, maxIdle, maxActive int) (*Cluster, error) {
	var addrs []string
	for _, raw := range strings.Split(nodes, ",") {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid Redis Cluster endpoint %q: %w", addr, err)
		}
		portNo, err := strconv.Atoi(port)
		if host == "" || err != nil || portNo < 1 || portNo > 65535 {
			return nil, fmt.Errorf("invalid Redis Cluster endpoint %q", addr)
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil, errors.New("Redis Cluster requires seed nodes")
	}
	options := []redis.DialOption{redis.DialConnectTimeout(5 * time.Second), redis.DialReadTimeout(5 * time.Second), redis.DialWriteTimeout(5 * time.Second), redis.DialPassword(password)}
	cluster := &redisc.Cluster{StartupNodes: addrs, DialOptions: options, PoolWaitTime: 5 * time.Second}
	cluster.CreatePool = func(addr string, opts ...redis.DialOption) (*redis.Pool, error) {
		return &redis.Pool{MaxIdle: maxIdle, MaxActive: maxActive, IdleTimeout: 5 * time.Minute, Wait: true,
			Dial: func() (redis.Conn, error) { return redis.Dial("tcp", addr, opts...) }}, nil
	}
	return &Cluster{cluster: cluster}, nil
}

func (c *Cluster) Close() error { return c.cluster.Close() }

// Conn is a lightweight facade. Every Do acquires a connection for its own key,
// so successive independent commands can target different cluster primaries.
func (c *Cluster) Conn() redis.Conn { return &conn{cluster: c} }

func (c *Cluster) Do(command string, args ...interface{}) (interface{}, error) {
	command = strings.ToUpper(command)
	switch command {
	case "MGET", "DEL", "UNLINK", "EXISTS":
		// These call sites read snapshots or remove independently owned keys.
		// Never split transactions, scripts, or commands requiring atomic writes.
		if len(args) > 1 {
			return c.split(command, args)
		}
	case "SELECT":
		if len(args) != 1 || fmt.Sprint(args[0]) != "0" {
			return nil, errors.New("Redis Cluster supports DB 0 only")
		}
		return "OK", nil
	case "MULTI", "EXEC", "WATCH", "UNWATCH", "DISCARD", "MSET", "MSETNX", "SCAN", "KEYS", "FLUSHDB", "FLUSHALL":
		return nil, fmt.Errorf("%s requires a cluster-aware call site", command)
	}
	reply, err := c.doOnce(command, args...)
	// READONLY is a server rejection before applying a write. Refresh topology
	// once and use a fresh connection; transport failures are never replayed.
	var serverErr redis.Error
	if errors.As(err, &serverErr) && (serverErr == "READONLY" || strings.HasPrefix(string(serverErr), "READONLY ")) {
		if refreshErr := c.cluster.Refresh(); refreshErr != nil {
			return nil, refreshErr
		}
		return c.doOnce(command, args...)
	}
	return reply, err
}

func (c *Cluster) doOnce(command string, args ...interface{}) (interface{}, error) {
	raw := c.cluster.Get()
	defer raw.Close()
	if command == "EVAL" || command == "EVALSHA" {
		if len(args) < 2 {
			return nil, errors.New("script requires a key count")
		}
		n, err := strconv.Atoi(fmt.Sprint(args[1]))
		if err != nil || n < 0 || len(args) < 2+n {
			return nil, errors.New("invalid script key count")
		}
		keys := make([]string, n)
		for i := range keys {
			keys[i], err = redis.String(args[2+i], nil)
			if err != nil {
				return nil, fmt.Errorf("invalid script key: %w", err)
			}
		}
		if err := redisc.BindConn(raw, keys...); err != nil {
			return nil, err
		}
	}
	// Retry only server-declared MOVED/ASK/TRYAGAIN rejection. A transport
	// timeout can follow an applied write, so never replay it automatically.
	retry, err := redisc.RetryConn(raw, 8, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer retry.Close()
	return normalizeReply(retry.Do(command, args...))
}

// Keep the redigo contract for callers that discard replies and inspect errors.
func normalizeReply(reply interface{}, err error) (interface{}, error) {
	if err == nil {
		if serverError, ok := reply.(redis.Error); ok {
			return nil, serverError
		}
	}
	return reply, err
}

func (c *Cluster) split(command string, args []interface{}) (interface{}, error) {
	// Preserve request order and nil entries. Reads are a best-effort view,
	// not an atomic snapshot spanning primaries.
	if command == "MGET" {
		values := make([]interface{}, len(args))
		for i, key := range args {
			// MGET returns nil for a non-string key, whereas GET reports
			// WRONGTYPE. Use single-key MGET to preserve that contract.
			v, err := redis.Values(c.Do("MGET", key))
			if err != nil {
				return nil, err
			}
			values[i] = v[0]
		}
		return values, nil
	}
	var total int64
	for _, key := range args {
		n, err := redis.Int64(c.Do(command, key))
		if err != nil {
			return nil, err
		}
		total += n
	}
	return total, nil
}

// ScanKeys visits every current primary and de-duplicates keys. Like SCAN on a
// standalone server, it is not a consistent snapshot while keys are changing.
func (c *Cluster) ScanKeys(pattern string) ([]string, error) {
	if err := c.cluster.Refresh(); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	err := c.cluster.EachNode(true, func(_ string, conn redis.Conn) error {
		cursor := 0
		for {
			values, err := redis.Values(conn.Do("SCAN", cursor, "MATCH", pattern, "COUNT", 200))
			if err != nil {
				return err
			}
			var keys []string
			if _, err := redis.Scan(values, &cursor, &keys); err != nil {
				return err
			}
			for _, key := range keys {
				seen[key] = struct{}{}
			}
			if cursor == 0 {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

type conn struct {
	cluster *Cluster
	closed  bool
}

func (c *conn) Close() error { c.closed = true; return nil }
func (c *conn) Err() error {
	if c.closed {
		return errors.New("Redis Cluster connection closed")
	}
	return nil
}
func (c *conn) Do(cmd string, args ...interface{}) (interface{}, error) {
	if err := c.Err(); err != nil {
		return nil, err
	}
	if cmd == "" {
		return nil, nil
	} // redigo pool's pending-reply cleanup
	return c.cluster.Do(cmd, args...)
}
func (c *conn) Send(string, ...interface{}) error {
	return errors.New("use Do for Redis Cluster commands")
}
func (c *conn) Flush() error { return errors.New("use Do for Redis Cluster commands") }
func (c *conn) Receive() (interface{}, error) {
	return nil, errors.New("use Do for Redis Cluster commands")
}

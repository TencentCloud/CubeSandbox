// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

func TestWriteNodeMetric_NilPool(t *testing.T) {
	cleanup := SetWriteNodeMetricHook(nil)
	defer cleanup()

	m := &NodeMetric{
		NodeID:        "node-1",
		MetricTime:    time.Now(),
		HasAllocated:  true,
		MilliCPUUsage: 1000,
	}
	if err := WriteNodeMetric(m); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
}

func TestWriteNodeMetric_EmptyMetrics(t *testing.T) {
	var called bool
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		called = true
		return nil
	})
	defer cleanup()

	m := &NodeMetric{NodeID: "node-1", MetricTime: time.Now()}
	if err := WriteNodeMetric(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	if called {
		t.Error("expected hook not to be called for empty metrics")
	}
}

func TestWriteNodeMetric_MissingNodeID(t *testing.T) {
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		return nil
	})
	defer cleanup()

	m := &NodeMetric{HasAllocated: true, MilliCPUUsage: 1000}
	if err := WriteNodeMetric(m); err == nil {
		t.Error("expected error for missing node id")
	}
}

func TestWriteNodeMetric_HookErrorPropagated(t *testing.T) {
	wantErr := errors.New("redis down")
	cleanup := SetWriteNodeMetricHook(func(m *NodeMetric) error {
		return wantErr
	})
	defer cleanup()

	m := &NodeMetric{NodeID: "node-1", MetricTime: time.Now(), HasAllocated: true, MilliCPUUsage: 1000}
	if err := WriteNodeMetric(m); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestParseRedisAddrs(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"10.0.0.1", 1},
		{"10.0.0.1,10.0.0.2", 2},
		{" 10.0.0.1 , 10.0.0.2 ", 2},
		{"10.0.0.1:26380,10.0.0.2", 2},
	}
	for _, tc := range cases {
		got := parseRedisAddrs(tc.input)
		if len(got) != tc.want {
			t.Errorf("parseRedisAddrs(%q) = %d addrs, want %d", tc.input, len(got), tc.want)
		}
	}
	// Bare host defaults to sentinel port 26379.
	addrs := parseRedisAddrs("10.0.0.1")
	if addrs[0] != "10.0.0.1:26379" {
		t.Errorf("bare host should default to :26379, got %s", addrs[0])
	}
}

func TestBuildRedisURLUsesConfiguredDatabase(t *testing.T) {
	if got := buildRedisURL("redis.example", 6380, 7, "secret"); got != "redis://:secret@redis.example:6380/7" {
		t.Fatalf("buildRedisURL() = %q, want configured database", got)
	}
	if got := buildRedisURL("redis.example", 0, 0, ""); got != "redis://redis.example:6379/0" {
		t.Fatalf("buildRedisURL() default = %q, want database 0", got)
	}
}

func TestResolveRedisConnectionPrefersURL(t *testing.T) {
	cfg := &config.Config{
		RedisURL:              "redis://url-user:url-pass@url.example:6380/7?pool_size=10",
		RedisHost:             "split.example",
		RedisPort:             6381,
		RedisDB:               3,
		RedisPassword:         "split-pass",
		RedisMasterName:       "mymaster",
		RedisSentinelNodes:    "sentinel.example:26379",
		RedisSentinelPassword: "sentinel-pass",
	}

	gotURL, sentinel, err := resolveRedisConnection(cfg)
	if err != nil {
		t.Fatalf("resolveRedisConnection: %v", err)
	}
	if sentinel {
		t.Fatal("resolveRedisConnection() selected Sentinel despite REDIS_URL")
	}
	if gotURL != cfg.RedisURL {
		t.Fatalf("resolveRedisConnection() URL = %q, want unchanged %q", gotURL, cfg.RedisURL)
	}
}

func TestDialSentinelSelectsConfiguredDatabase(t *testing.T) {
	sentinelLn := listenTestRedis(t)
	masterLn := listenTestRedis(t)
	masterDB := make(chan int, 1)

	go serveTestSentinel(t, sentinelLn, masterLn.Addr().(*net.TCPAddr).Port)
	go serveTestMaster(t, masterLn, masterDB)

	cfg := &config.Config{
		RedisMasterName:    "mymaster",
		RedisSentinelNodes: sentinelLn.Addr().String(),
		RedisDB:            7,
	}
	conn, err := dialSentinel(cfg)
	if err != nil {
		t.Fatalf("dialSentinel: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-masterDB:
		if got != 7 {
			t.Fatalf("master selected database %d, want 7", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for master SELECT")
	}
}

func listenTestRedis(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func serveTestSentinel(t *testing.T, ln net.Listener, masterPort int) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	cmd, err := readTestRedisCommand(conn)
	if err != nil || len(cmd) != 3 || strings.ToUpper(cmd[0]) != "SENTINEL" {
		return
	}
	fmt.Fprintf(conn, "*2\r\n$9\r\n127.0.0.1\r\n$%d\r\n%d\r\n", len(strconv.Itoa(masterPort)), masterPort)
}

func serveTestMaster(t *testing.T, ln net.Listener, selectedDB chan<- int) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	cmd, err := readTestRedisCommand(conn)
	if err != nil || len(cmd) != 2 || strings.ToUpper(cmd[0]) != "SELECT" {
		return
	}
	db, err := strconv.Atoi(cmd[1])
	if err != nil {
		return
	}
	selectedDB <- db
	fmt.Fprint(conn, "+OK\r\n")
}

func readTestRedisCommand(r net.Conn) ([]string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected RESP header %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	cmd := make([]string, 0, count)
	for i := 0; i < count; i++ {
		line, err = br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(line, "$") {
			return nil, fmt.Errorf("unexpected RESP bulk header %q", line)
		}
		length, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		value := make([]byte, length+2)
		if _, err := io.ReadFull(br, value); err != nil {
			return nil, err
		}
		cmd = append(cmd, string(value[:length]))
	}
	return cmd, nil
}

func TestPing_NilPool(t *testing.T) {
	saved := pool
	pool = nil
	defer func() { pool = saved }()

	if err := Ping(context.Background()); err == nil {
		t.Error("expected error for nil pool")
	}
}

func TestPing_UnreachablePool(t *testing.T) {
	saved := pool
	pool = &redis.Pool{
		Dial: func() (redis.Conn, error) {
			return redis.Dial("tcp", "127.0.0.1:1", // unused port
				redis.DialConnectTimeout(100*time.Millisecond),
			)
		},
	}
	defer func() { pool = saved }()

	if err := Ping(context.Background()); err == nil {
		t.Error("expected error for unreachable redis")
	}
}

func TestReadNodeMetric_EmptyNodeID(t *testing.T) {
	if _, err := ReadNodeMetric(""); err == nil {
		t.Error("expected error for empty node id")
	}
}

func TestReadNodeMetric_NilPool(t *testing.T) {
	saved := pool
	pool = nil
	defer func() { pool = saved }()

	m, err := ReadNodeMetric("node-1")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil metric for nil pool, got %+v", m)
	}
}

func TestReadNodeMetric_Hook(t *testing.T) {
	want := &NodeMetric{
		NodeID:              "node-1",
		MetricTime:          time.Now().Truncate(time.Second),
		HasAllocated:        true,
		MilliCPUUsage:       2000,
		MemoryMBUsage:       2048,
		MvmNum:              3,
		NicQueues:           4,
		HasDisk:             true,
		DataDiskUsagePer:    55.5,
		StorageDiskUsagePer: 66.6,
		SysDiskUsagePer:     77.7,
	}
	cleanup := SetReadNodeMetricHook(func(nodeID string) (*NodeMetric, error) {
		if nodeID != "node-1" {
			t.Errorf("nodeID = %q, want node-1", nodeID)
		}
		return want, nil
	})
	defer cleanup()

	got, err := ReadNodeMetric("node-1")
	if err != nil {
		t.Fatalf("ReadNodeMetric: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil metric")
	}
	if got.MvmNum != 3 || got.MilliCPUUsage != 2000 || got.DataDiskUsagePer != 55.5 {
		t.Errorf("metric = %+v, want %+v", got, want)
	}
}

func TestReadNodeMetric_HookErrorPropagated(t *testing.T) {
	wantErr := errors.New("redis read failed")
	cleanup := SetReadNodeMetricHook(func(nodeID string) (*NodeMetric, error) {
		return nil, wantErr
	})
	defer cleanup()

	if _, err := ReadNodeMetric("node-1"); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestReadNodeMetric_HookMiss(t *testing.T) {
	cleanup := SetReadNodeMetricHook(func(nodeID string) (*NodeMetric, error) {
		return nil, nil // Redis miss
	})
	defer cleanup()

	m, err := ReadNodeMetric("node-1")
	if err != nil {
		t.Fatalf("expected nil err on miss, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil metric on miss, got %+v", m)
	}
}

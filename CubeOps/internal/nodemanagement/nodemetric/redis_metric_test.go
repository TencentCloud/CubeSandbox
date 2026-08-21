// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
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

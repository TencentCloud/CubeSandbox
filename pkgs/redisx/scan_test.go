// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisx

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

type scanFixture struct {
	sync.Mutex
	addresses map[string]int
}

type scanConn struct {
	promotionConn
	scans *scanFixture
}

func (c *scanConn) Do(command string, args ...interface{}) (interface{}, error) {
	switch command {
	case "":
		return nil, nil
	case "CLUSTER":
		return []interface{}{
			[]interface{}{int64(0), int64(8191), []interface{}{[]byte("primary-a"), int64(6379)}, []interface{}{[]byte("replica-a"), int64(6379)}},
			[]interface{}{int64(8192), int64(16383), []interface{}{[]byte("primary-b"), int64(6379)}, []interface{}{[]byte("replica-b"), int64(6379)}},
		}, nil
	case "SCAN":
		c.scans.Lock()
		defer c.scans.Unlock()
		c.scans.addresses[c.addr]++
		return []interface{}{[]byte("0"), []interface{}{[]byte(c.addr), []byte("shared-key")}}, nil
	default:
		return nil, fmt.Errorf("unexpected command %s", command)
	}
}

func TestScanUsesOnlyPrimaryPools(t *testing.T) {
	f := &scanFixture{addresses: make(map[string]int)}
	raw := &redisc.Cluster{StartupNodes: []string{"primary-a:6379"}}
	raw.CreatePool = func(addr string, _ ...redis.DialOption) (*redis.Pool, error) {
		return &redis.Pool{Dial: func() (redis.Conn, error) {
			return &scanConn{promotionConn: promotionConn{addr: addr}, scans: f}, nil
		}}, nil
	}
	c := &Cluster{cluster: raw}
	defer c.Close()
	keys, err := c.ScanKeys("*")
	if err != nil {
		t.Fatal(err)
	}
	f.Lock()
	defer f.Unlock()
	want := map[string]int{"primary-a:6379": 1, "primary-b:6379": 1}
	if !reflect.DeepEqual(f.addresses, want) {
		t.Fatalf("SCAN endpoints = %v; want %v", f.addresses, want)
	}
	if !reflect.DeepEqual(keys, []string{"primary-a:6379", "primary-b:6379", "shared-key"}) {
		t.Fatalf("unexpected merged keys: %v", keys)
	}
}

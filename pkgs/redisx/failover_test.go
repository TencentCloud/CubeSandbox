// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisx

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

type promotionFixture struct {
	sync.Mutex
	primary          string
	rejection        error
	promote          bool
	attempts, writes int
}
type promotionConn struct {
	addr    string
	fixture *promotionFixture
}

func (c *promotionConn) Close() error                      { return nil }
func (c *promotionConn) Err() error                        { return nil }
func (c *promotionConn) Send(string, ...interface{}) error { return errors.New("unexpected pipeline") }
func (c *promotionConn) Flush() error                      { return nil }
func (c *promotionConn) Receive() (interface{}, error)     { return nil, errors.New("unexpected receive") }
func (c *promotionConn) Do(command string, args ...interface{}) (interface{}, error) {
	f := c.fixture
	f.Lock()
	defer f.Unlock()
	switch command {
	case "":
		return nil, nil
	case "CLUSTER":
		return []interface{}{[]interface{}{int64(0), int64(16383), []interface{}{[]byte(f.primary), int64(6379)}}}, nil
	case "INCR":
		f.attempts++
		if c.addr == "old:6379" {
			if f.promote {
				f.primary = "new"
			}
			return nil, f.rejection
		}
		f.writes++
		return int64(f.writes), nil
	default:
		return nil, fmt.Errorf("unexpected command %s", command)
	}
}

func TestReadOnlyRefreshDoesNotReplayAmbiguousWrites(t *testing.T) {
	for _, tc := range []struct {
		name             string
		rejection        error
		promote          bool
		attempts, writes int
	}{
		{"promoted primary", redis.Error("READONLY demoted primary"), true, 2, 1},
		{"bounded replica rejection", redis.Error("READONLY still a replica"), false, 2, 0},
		{"ambiguous transport failure", errors.New("timeout after write"), true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &promotionFixture{primary: "old", rejection: tc.rejection, promote: tc.promote}
			raw := &redisc.Cluster{StartupNodes: []string{"old:6379"}}
			raw.CreatePool = func(addr string, _ ...redis.DialOption) (*redis.Pool, error) {
				return &redis.Pool{Dial: func() (redis.Conn, error) { return &promotionConn{addr, f}, nil }}, nil
			}
			c := &Cluster{cluster: raw}
			defer c.Close()
			if err := raw.Refresh(); err != nil {
				t.Fatal(err)
			}
			value, err := redis.Int(c.Do("INCR", "promotion-key"))
			if tc.writes > 0 {
				if err != nil || value != 1 {
					t.Fatalf("promotion: value=%d err=%v", value, err)
				}
			} else if err == nil {
				t.Fatal("server/transport error was lost")
			}
			f.Lock()
			defer f.Unlock()
			if f.attempts != tc.attempts || f.writes != tc.writes {
				t.Fatalf("attempts=%d writes=%d; want %d/%d", f.attempts, f.writes, tc.attempts, tc.writes)
			}
		})
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisx

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

func liveClient(t *testing.T) *Cluster {
	t.Helper()
	if os.Getenv("TEST_REDIS_CLUSTER_NODES") == "" {
		t.Skip("isolated Cluster fixture is not configured")
	}
	c, err := NewCluster(os.Getenv("TEST_REDIS_CLUSTER_NODES"), os.Getenv("TEST_REDIS_CLUSTER_PASSWORD"), 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func liveDial(addr string) (redis.Conn, error) {
	return redis.Dial("tcp", addr, redis.DialPassword(os.Getenv("TEST_REDIS_CLUSTER_PASSWORD")), redis.DialReadTimeout(8*time.Second))
}

func TestLiveWritesDuringPromotion(t *testing.T) {
	if os.Getenv("TEST_REDIS_CLUSTER_FAILOVER") != "1" {
		t.Skip("isolated failover fixture is not enabled")
	}
	c := liveClient(t)
	raw, err := liveDial(strings.Split(os.Getenv("TEST_REDIS_CLUSTER_NODES"), ",")[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	slots, err := redis.Values(raw.Do("CLUSTER", "SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := redis.Values(slots[0], nil)
	lo, _ := redis.Int(entry[0], nil)
	hi, _ := redis.Int(entry[1], nil)
	if len(entry) < 4 {
		t.Fatal("primary has no replica")
	}
	owner, _ := redis.Values(entry[2], nil)
	follower, _ := redis.Values(entry[3], nil)
	address := func(v []interface{}) string {
		host, _ := redis.String(v[0], nil)
		port, _ := redis.Int(v[1], nil)
		return fmt.Sprintf("%s:%d", host, port)
	}
	primary, err := liveDial(address(owner))
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := liveDial(address(follower))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	key := ""
	for i := 0; ; i++ {
		key = fmt.Sprintf("cube-live-flow:%d:%d", time.Now().UnixNano(), i)
		slot := redisc.Slot(key)
		if slot >= lo && slot <= hi {
			break
		}
	}
	if _, err = c.Do("SET", key, 0); err != nil {
		t.Fatal(err)
	}
	defer c.Do("DEL", key)
	trigger := make(chan struct{})
	done := make(chan []string)
	go func() {
		failures := []string{}
		for i := 0; i < 100; i++ {
			if _, e := c.Do("INCR", key); e != nil {
				failures = append(failures, e.Error())
			}
			if i == 9 {
				close(trigger)
			}
			time.Sleep(10 * time.Millisecond)
		}
		done <- failures
	}()
	<-trigger
	if _, err = replica.Do("CLUSTER", "FAILOVER"); err != nil {
		t.Fatal(err)
	}
	failures := <-done
	deadline := time.Now().Add(10 * time.Second)
	for {
		role, e := redis.Values(primary.Do("ROLE"))
		if e != nil {
			t.Fatal(e)
		}
		name, _ := redis.String(role[0], nil)
		if name == "slave" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("promotion timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, rejection := primary.Do("INCR", key)
	if rejection == nil {
		t.Fatal("demoted primary accepted direct write")
	}
	value, err := redis.Int(c.Do("GET", key))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("primary=%s promoted=%s slot=%d direct_demoted_reply=%q successful_writes=%d counter=%d errors=%v", address(owner), address(follower), redisc.Slot(key), rejection.Error(), 100-len(failures), value, failures)
	if len(failures) != 0 || value != 100 {
		t.Fatalf("continuous writes did not complete exactly once")
	}
}

func request(r *bufio.Reader) ([]interface{}, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("expected array")
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, count)
	for i := range out {
		line, err = r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, e := strconv.Atoi(strings.TrimSpace(line[1:]))
		if e != nil {
			return nil, e
		}
		data := make([]byte, size+2)
		if _, e = io.ReadFull(r, data); e != nil {
			return nil, e
		}
		out[i] = data[:size]
	}
	return out, nil
}

func reply(w io.Writer, v interface{}) error {
	switch x := v.(type) {
	case nil:
		_, e := io.WriteString(w, "$-1\r\n")
		return e
	case redis.Error:
		_, e := fmt.Fprintf(w, "-%s\r\n", x)
		return e
	case int64:
		_, e := fmt.Fprintf(w, ":%d\r\n", x)
		return e
	case string:
		_, e := fmt.Fprintf(w, "+%s\r\n", x)
		return e
	case []byte:
		_, e := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(x), x)
		return e
	case []interface{}:
		if _, e := fmt.Fprintf(w, "*%d\r\n", len(x)); e != nil {
			return e
		}
		for _, v := range x {
			if e := reply(w, v); e != nil {
				return e
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported reply %T", v)
	}
}

func TestLiveAmbiguousTransportIsNotReplayed(t *testing.T) {
	for _, mode := range []string{"disconnect", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			direct := liveClient(t)
			key := fmt.Sprintf("cube-live-ambiguous:%s:%d", mode, time.Now().UnixNano())
			if _, e := direct.Do("SET", key, 0); e != nil {
				t.Fatal(e)
			}
			defer direct.Do("DEL", key)
			fault := liveClient(t)
			var applied atomic.Int64
			var mu sync.Mutex
			var listeners []net.Listener
			defer func() {
				for _, l := range listeners {
					l.Close()
				}
			}()
			fault.cluster.CreatePool = func(addr string, opts ...redis.DialOption) (*redis.Pool, error) {
				listener, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					return nil, e
				}
				mu.Lock()
				listeners = append(listeners, listener)
				mu.Unlock()
				go func() {
					for {
						client, e := listener.Accept()
						if e != nil {
							return
						}
						go func() {
							defer client.Close()
							backend, e := liveDial(addr)
							if e != nil {
								return
							}
							defer backend.Close()
							reader := bufio.NewReader(client)
							for {
								parts, e := request(reader)
								if e != nil {
									return
								}
								command := strings.ToUpper(string(parts[0].([]byte)))
								value, e := backend.Do(command, parts[1:]...)
								if command == "INCR" && e == nil {
									applied.Add(1)
									if mode == "timeout" {
										time.Sleep(6 * time.Second)
									}
									return
								}
								if e != nil {
									if server, ok := e.(redis.Error); ok {
										value = server
									} else {
										return
									}
								}
								if reply(client, value) != nil {
									return
								}
							}
						}()
					}
				}()
				return &redis.Pool{MaxIdle: 2, MaxActive: 8, Dial: func() (redis.Conn, error) { return redis.Dial("tcp", listener.Addr().String(), opts...) }}, nil
			}
			started := time.Now()
			_, err := fault.Do("INCR", key)
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("transport error hidden")
			}
			count, e := redis.Int(direct.Do("GET", key))
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("fault=%s error=%q elapsed=%s applied_writes=%d counter=%d", mode, err.Error(), elapsed, applied.Load(), count)
			if count != 1 || applied.Load() != 1 {
				t.Fatal("ambiguous write replayed")
			}
		})
	}
}

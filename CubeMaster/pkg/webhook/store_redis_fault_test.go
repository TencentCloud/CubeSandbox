// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// RESP proxy is test-only. The upstream reply is consumed before dropping it,
// proving execution happened (rather than substituting connection refusal).
func respWrite(w io.Writer, v interface{}) {
	switch x := v.(type) {
	case nil:
		fmt.Fprint(w, "$-1\r\n")
	case redis.Error:
		fmt.Fprintf(w, "-%s\r\n", x)
	case error:
		fmt.Fprint(w, "-ERR proxy upstream\r\n")
	case string:
		fmt.Fprintf(w, "$%d\r\n%s\r\n", len(x), x)
	case []byte:
		fmt.Fprintf(w, "$%d\r\n", len(x))
		w.Write(x)
		fmt.Fprint(w, "\r\n")
	case int64:
		fmt.Fprintf(w, ":%d\r\n", x)
	case []interface{}:
		fmt.Fprintf(w, "*%d\r\n", len(x))
		for _, e := range x {
			respWrite(w, e)
		}
	default:
		panic(fmt.Sprintf("unexpected RESP %T", x))
	}
}
func respServer(t *testing.T, handler func(string, []interface{}) (interface{}, bool)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var clients []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				r := redis.NewConn(c, 0, 0)
				w := bufio.NewWriter(c)
				for {
					v, e := redis.Values(r.Receive())
					if e != nil {
						return
					}
					name, e := redis.String(v[0], nil)
					if e != nil {
						return
					}
					reply, closeNow := handler(strings.ToUpper(name), v[1:])
					if closeNow {
						return
					}
					respWrite(w, reply)
					if w.Flush() != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range clients {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}
func faultProxy(t *testing.T, conf config.RedisConf) (string, *atomic.Int32) {
	t.Helper()
	up, err := redis.Dial("tcp", conf.Nodes, redis.DialPassword(conf.Password), redis.DialDatabase(conf.DbNo))
	require.NoError(t, err)
	t.Cleanup(func() { up.Close() })
	var mu sync.Mutex
	mode := &atomic.Int32{}
	addr := respServer(t, func(cmd string, args []interface{}) (interface{}, bool) {
		mu.Lock()
		defer mu.Unlock()
		script := cmd == "EVAL" || cmd == "EVALSHA"
		if script && mode.Load() == 2 {
			mode.Store(0)
			return nil, true
		}
		v, e := up.Do(cmd, args...)
		if e != nil {
			return e, false
		}
		if script && mode.CompareAndSwap(1, 0) {
			return nil, true
		}
		return v, false
	})
	return addr, mode
}
func TestRedisLostResponses(t *testing.T) {
	for _, op := range []string{"put", "claim", "complete", "retry"} {
		t.Run(op, func(t *testing.T) {
			s, conf := newTestStore(t, testOptions())
			addr, mode := faultProxy(t, conf)
			conf.Nodes = addr
			proxy, err := NewRedisDeliveryStore(conf, testOptions())
			require.NoError(t, err)
			defer proxy.Close()
			proxy.keys = s.keys
			task := testTask()
			now := task.NextAttemptAt
			if op == "put" {
				mode.Store(1)
				require.ErrorIs(t, proxy.Put(testCtx, task), ErrStoreUncertain)
				first := claimOne(t, s, now)
				require.NoError(t, proxy.Put(testCtx, task))
				require.Equal(t, 1, record(t, s, first.Receipt.key).Attempts)
			} else {
				require.NoError(t, s.Put(testCtx, task))
				if op == "claim" {
					mode.Store(1)
					_, err = proxy.Claim(testCtx, now, 1, time.Second)
					require.ErrorIs(t, err, ErrStoreUncertain)
					v, e := s.Claim(testCtx, now, 1, time.Second)
					require.NoError(t, e)
					require.Empty(t, v)
					c := claimOne(t, s, now.Add(time.Second))
					require.Equal(t, 2, c.Attempts)
				} else {
					c := claimOne(t, s, now)
					mode.Store(1)
					if op == "complete" {
						require.ErrorIs(t, proxy.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStoreUncertain)
						require.ErrorIs(t, s.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStaleReceipt)
						require.Equal(t, "succeeded", record(t, s, c.Receipt.key).State)
					} else {
						require.ErrorIs(t, proxy.Retry(testCtx, c.Receipt, now.Add(time.Hour), FailureSummary{Code: "timeout"}), ErrStoreUncertain)
						require.ErrorIs(t, s.Retry(testCtx, c.Receipt, now, FailureSummary{}), ErrStaleReceipt)
						r := record(t, s, c.Receipt.key)
						require.Equal(t, now.Add(time.Hour).UnixMilli(), r.Next)
						require.Equal(t, 1, r.Attempts)
					}
				}
			}
			require.Equal(t, int64(1), command(t, s, "HLEN", s.keys[0]))
		})
	}
	t.Run("uncommitted complete recovers", func(t *testing.T) {
		s, conf := newTestStore(t, testOptions())
		addr, mode := faultProxy(t, conf)
		conf.Nodes = addr
		proxy, err := NewRedisDeliveryStore(conf, testOptions())
		require.NoError(t, err)
		defer proxy.Close()
		proxy.keys = s.keys
		task := testTask()
		require.NoError(t, s.Put(testCtx, task))
		c := claimOne(t, s, task.NextAttemptAt)
		mode.Store(2)
		require.ErrorIs(t, proxy.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStoreUncertain)
		next := claimOne(t, s, c.LeaseUntil)
		require.Equal(t, 2, next.Attempts)
	})
}
func TestRedisPoolAndCancellation(t *testing.T) {
	o := testOptions()
	o.MaxActive = 1
	o.MaxIdle = 1
	o.OperationTimeout = 80 * time.Millisecond
	s, _ := newTestStore(t, o)
	ctx, cancel := context.WithTimeout(testCtx, time.Second)
	defer cancel()
	held, err := s.pool.GetContext(ctx)
	require.NoError(t, err)
	start := time.Now()
	require.ErrorIs(t, s.Put(testCtx, testTask()), ErrStoreUnavailable)
	require.Less(t, time.Since(start), 400*time.Millisecond)
	held.Close()
	require.NoError(t, s.Put(testCtx, testTask()))
	// A peer that accepts TCP but never responds exercises AUTH and command I/O.
	release := make(chan struct{})
	addr := respServer(t, func(string, []interface{}) (interface{}, bool) { <-release; return nil, true })
	slow, err := NewRedisDeliveryStore(config.RedisConf{Nodes: addr, Password: "auth"}, o)
	require.NoError(t, err)
	baseline := runtime.NumGoroutine()
	for i := 0; i < 6; i++ {
		start = time.Now()
		err = slow.Put(testCtx, testTask())
		require.ErrorIs(t, err, ErrStoreUnavailable)
		require.Less(t, time.Since(start), 400*time.Millisecond)
	}
	close(release)
	slow.Close()
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= baseline+2 }, time.Second, 10*time.Millisecond)
	// Already-dialed connections must cancel commands, too.
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	addr = respServer(t, func(cmd string, _ []interface{}) (interface{}, bool) {
		if cmd == "SELECT" {
			return "OK", false
		}
		started <- struct{}{}
		<-block
		return nil, true
	})
	slow, err = NewRedisDeliveryStore(config.RedisConf{Nodes: addr}, o)
	require.NoError(t, err)
	callCtx, stop := context.WithCancel(testCtx)
	done := make(chan error, 1)
	go func() { done <- slow.Put(callCtx, testTask()) }()
	<-started
	stop()
	err = <-done
	require.ErrorIs(t, err, context.Canceled)
	close(block)
	slow.Close()
}
func TestRedisSentinelAndReadonly(t *testing.T) {
	s, conf := newTestStore(t, testOptions())
	host, port, err := net.SplitHostPort(conf.Nodes)
	require.NoError(t, err)
	var lookups atomic.Int32
	var auths atomic.Int32
	sentinel := respServer(t, func(cmd string, args []interface{}) (interface{}, bool) {
		if cmd == "AUTH" {
			pwd, _ := redis.String(args[0], nil)
			if pwd != "sentinel-only" {
				return redis.Error("WRONGPASS sentinel"), false
			}
			auths.Add(1)
			return "OK", false
		}
		if cmd == "SENTINEL" {
			lookups.Add(1)
			return []interface{}{host, port}, false
		}
		return redis.Error("ERR unexpected"), false
	})
	o := testOptions()
	o.MaxIdle = 0
	conf.MasterName = "master"
	conf.SentinelNodes = "127.0.0.1:1," + sentinel
	conf.SentinelPassword = "sentinel-only"
	original := conf
	via, err := NewRedisDeliveryStore(conf, o)
	require.NoError(t, err)
	defer via.Close()
	via.keys = s.keys
	require.NoError(t, via.Put(testCtx, testTask()))
	require.NoError(t, via.Put(testCtx, testTask()))
	require.Equal(t, original, conf)
	require.Equal(t, int32(2), lookups.Load())
	require.Equal(t, int32(2), auths.Load())
	conf.SentinelPassword = "wrong"
	bad, err := NewRedisDeliveryStore(conf, o)
	require.NoError(t, err)
	require.ErrorIs(t, bad.Put(testCtx, testTask()), ErrStoreUnavailable)
	bad.Close()
	// READONLY is a server response with no transport error: ensure it still
	// removes the old socket from the pool and the next dial resolves again.
	var calls atomic.Int32
	endpoint := respServer(t, func(cmd string, _ []interface{}) (interface{}, bool) {
		if cmd == "AUTH" || cmd == "SELECT" {
			return "OK", false
		}
		calls.Add(1)
		return redis.Error("READONLY demoted master"), false
	})
	conf = original
	conf.MasterName = ""
	conf.Nodes = endpoint
	conf.SentinelNodes = ""
	conf.SentinelPassword = ""
	ro, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	defer ro.Close()
	require.ErrorIs(t, ro.Put(testCtx, testTask()), ErrStoreUnavailable)
	require.Zero(t, ro.pool.Stats().IdleCount)
	require.ErrorIs(t, ro.Put(testCtx, testTask()), ErrStoreUnavailable)
	require.Equal(t, int32(2), calls.Load())
}

func TestRedisTotalBudgetAndScriptFallback(t *testing.T) {
	o := testOptions()
	o.OperationTimeout = 100 * time.Millisecond
	// NOSCRIPT must fall back, but cannot grant the EVAL another full budget.
	release := make(chan struct{})
	var eval atomic.Int32
	endpoint := respServer(t, func(cmd string, _ []interface{}) (interface{}, bool) {
		if cmd == "EVALSHA" {
			time.Sleep(60 * time.Millisecond)
			return redis.Error("NOSCRIPT cache missing"), false
		}
		if cmd == "EVAL" {
			eval.Add(1)
			<-release
			return nil, true
		}
		return "OK", false
	})
	s, err := NewRedisDeliveryStore(config.RedisConf{Nodes: endpoint}, o)
	require.NoError(t, err)
	start := time.Now()
	require.ErrorIs(t, s.Put(testCtx, testTask()), ErrStoreUncertain)
	elapsed := time.Since(start)
	close(release)
	s.Close()
	require.Equal(t, int32(1), eval.Load())
	require.Less(t, elapsed, 150*time.Millisecond)
	// Multiple failed Sentinel addresses share the deadline, including AUTH.
	sentinelRelease := make(chan struct{})
	var later atomic.Int32
	first := respServer(t, func(string, []interface{}) (interface{}, bool) { <-sentinelRelease; return nil, true })
	second := respServer(t, func(string, []interface{}) (interface{}, bool) { later.Add(1); return nil, true })
	s, err = NewRedisDeliveryStore(config.RedisConf{MasterName: "master", SentinelNodes: first + "," + second, SentinelPassword: "independent"}, o)
	require.NoError(t, err)
	start = time.Now()
	require.ErrorIs(t, s.Put(testCtx, testTask()), ErrStoreUnavailable)
	elapsed = time.Since(start)
	close(sentinelRelease)
	s.Close()
	require.Less(t, elapsed, 150*time.Millisecond)
	require.Zero(t, later.Load())
}
func TestRedisAuthenticationAndDatabase(t *testing.T) {
	s, conf := newTestStore(t, testOptions())
	task := testTask()
	require.NoError(t, s.Put(testCtx, task))
	c, err := redis.Dial("tcp", conf.Nodes, redis.DialPassword(conf.Password), redis.DialDatabase(0))
	require.NoError(t, err)
	defer c.Close()
	n, err := redis.Int(c.Do("EXISTS", s.keys[0]))
	require.NoError(t, err)
	require.Zero(t, n)
	conf.Password = "deliberately-wrong"
	wrong, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	defer wrong.Close()
	require.ErrorIs(t, wrong.Put(testCtx, task), ErrStoreUnavailable)
}

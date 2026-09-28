// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package wrapredis provides a wrapper for redis.
package wrapredis

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type RedisWrap struct {
	RedisConnPool *redis.Pool
	ModuleName    string
	Addr          string
	redisConf     *config.RedisConf
	connectPeak   int64
}

const (
	redisPoolKey    = "redis"
	dialTimeout     = 5 * time.Second
	dialMaxAttempts = 3
	dialRetryWait   = 200 * time.Millisecond
)

var (
	safeMap sync.Map
	mutex   sync.Mutex
)

func GetRedis() *RedisWrap {
	r, ok := safeMap.Load(redisPoolKey)
	if ok {
		return r.(*RedisWrap)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if config.GetConfig().RedisConf == nil {
		panic("redis conf is nil")
	}

	r, ok = safeMap.Load(redisPoolKey)
	if ok {
		return r.(*RedisWrap)
	}

	v := GetRedisConnPoolWrap(redisPoolKey, config.GetConfig().RedisConf)
	safeMap.Store(redisPoolKey, v)
	return v
}

func GetRedisConnPoolWrap(caller string, redisConf *config.RedisConf) *RedisWrap {
	if redisConf == nil {
		return nil
	}
	if redisConf.MaxRetry == 0 {
		redisConf.MaxRetry = 3
	}
	redisW := &RedisWrap{
		ModuleName: caller,
		Addr:       redisDisplayAddr(redisConf),
		redisConf:  redisConf,
		RedisConnPool: &redis.Pool{
			MaxIdle:     redisConf.MaxIdle,
			MaxActive:   redisConf.MaxActive,
			IdleTimeout: time.Duration(redisConf.IdleTimeout) * time.Second,
			Wait:        true,

			TestOnBorrow: func(c redis.Conn, t time.Time) error {
				if time.Since(t) < 5*time.Second {
					return nil
				}
				// Return the error (do not Fatalf): during Sentinel failover the
				// pool may still hold connections to the old master, and PING
				// failure is expected. Do()'s retry loop closes bad conns and
				// Dial() re-resolves the current master.
				_, err := c.Do("PING")
				return err
			},
		},
	}
	redisW.RedisConnPool.Dial = redisW.Dial
	go redisW.reportMetric()
	return redisW
}

// isReadonlyErr reports a Redis READONLY reply. After Sentinel failover the
// demoted replica still answers PING, so TestOnBorrow keeps pooled conns, but
// writes fail with READONLY until we re-resolve the master.
func isReadonlyErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "READONLY")
}

// normalizeDoReply maps Redis ERROR replies to Go errors. redigo returns
// `-ERR ...` as (redis.Error, nil); wrapredis callers check the error value.
func normalizeDoReply(reply interface{}, err error) (interface{}, error) {
	if err != nil {
		return reply, err
	}
	if e, ok := reply.(redis.Error); ok {
		return nil, e
	}
	return reply, nil
}

func (r *RedisWrap) Do(cmd string, args ...interface{}) (reply interface{}, err error) {
	readonlyRetried := false
	for i := 0; i < r.redisConf.MaxRetry; i++ {
		conn := r.RedisConnPool.Get()
		if err = conn.Err(); err != nil {
			_ = conn.Close()
			continue
		}
		reply, err = normalizeDoReply(conn.Do(cmd, args...))
		if err != nil {
			// PING succeeds on a demoted replica, so poison the pooled conn
			// (QUIT) before Close; otherwise the pool keeps recycling it.
			if isReadonlyErr(err) {
				_, _ = conn.Do("QUIT")
				_ = conn.Close()
				// Mirror Lua: at most one Sentinel re-resolve + retry on READONLY.
				if !readonlyRetried && r.redisConf.MasterName != "" {
					readonlyRetried = true
					fresh, dialErr := r.Dial()
					if dialErr != nil {
						err = dialErr
						continue
					}
					reply, err = normalizeDoReply(fresh.Do(cmd, args...))
					_ = fresh.Close()
					if err == nil {
						return reply, nil
					}
					// Don't burn MaxRetry on a deterministic server error from
					// the new master (e.g. WRONGTYPE); return it immediately.
					if _, ok := err.(redis.Error); ok {
						return nil, err
					}
				}
				continue
			}
			_ = conn.Close()
			// Deterministic Redis ERROR replies (WRONGTYPE, ERR, …) must not
			// burn MaxRetry or churn the pool — only transport failures retry.
			if _, ok := err.(redis.Error); ok {
				return nil, err
			}
			continue
		}
		_ = conn.Close()
		return reply, nil
	}
	return reply, err
}

func (r *RedisWrap) Dial() (c redis.Conn, err error) {
	defer func() {
		if err == nil {
			atomic.AddInt64(&r.connectPeak, 1)
		}
	}()

	for i := 0; i < dialMaxAttempts; i++ {
		if i > 0 {
			time.Sleep(dialRetryWait)
		}
		addr, resolveErr := resolveRedisAddr(r.redisConf)
		if resolveErr != nil {
			// Fail fast on resolution errors: in Sentinel mode
			// lookupSentinelMaster already retries across every sentinel, so a
			// failure here means all sentinels are unreachable. Retrying the
			// outer loop would only repeat the same exhausted probe.
			return nil, resolveErr
		}
		c, err = newConn(addr, r.redisConf.Password, r.redisConf.DbNo)
		if err != nil {
			continue
		}
		return c, nil
	}
	return nil, errors.New("redis连接失败")
}

func (r *RedisWrap) reportMetric() {
	metricTrace := &CubeLog.RequestTrace{
		Caller: r.ModuleName,
		Callee: "metric",
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		recov.WithRecover(func() {
			if config.GetConfig().Common.ReportLocalCreateNum {
				redisstat := r.RedisConnPool.Stats()
				if redisstat.ActiveCount > 0 {
					metricTrace.Action = "ActiveCount"
					metricTrace.RetCode = int64(redisstat.ActiveCount)
					CubeLog.Trace(metricTrace)
				}
				if redisstat.IdleCount > 0 {
					metricTrace.Action = "IdleCount"
					metricTrace.RetCode = int64(redisstat.IdleCount)
					CubeLog.Trace(metricTrace)
				}
				if v := atomic.SwapInt64(&r.connectPeak, 0); v > 0 {
					metricTrace.Action = "connectPeak"
					metricTrace.RetCode = v
					CubeLog.Trace(metricTrace)
				}
			}
		}, func(panicError interface{}) {
			CubeLog.WithContext(context.Background()).Fatalf("RedisWrap reportMetric panic:%v", panicError)
		})
	}
}

func newConn(serviceName string, passwd string, db int) (redis.Conn, error) {
	CubeLog.Debugf("redis连接地址:%s", serviceName)
	c, err := redis.Dial("tcp", serviceName,
		redis.DialConnectTimeout(dialTimeout),
		redis.DialReadTimeout(dialTimeout),
		redis.DialDatabase(db),
		redis.DialPassword(passwd))
	if err != nil {
		// Do not Fatalf: Dial()'s retry loop must be able to continue during
		// Sentinel failover when the newly advertised master is not ready yet.
		CubeLog.Errorf("连接redis:%s 失败:%s", serviceName, err)
		return c, err
	}
	return c, err
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package wrapredis provides a wrapper for redis.
package wrapredis

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type RedisWrap struct {
	RedisConnPool *redis.Pool
	ModuleName    string
	Addr          string
	redisConf     *config.RedisConf
	connectPeak   int64
}

const redisPoolKey = "redis"

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
		Addr:       redisConf.Nodes,
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
				_, err := c.Do("PING")
				if err != nil {
					CubeLog.Fatalf("redis ping 失败:%s", err)
				}
				return err
			},
		},
	}
	redisW.RedisConnPool.Dial = redisW.Dial
	go redisW.reportMetric()
	return redisW
}

func (r *RedisWrap) Do(cmd string, args ...interface{}) (reply interface{}, err error) {
	for i := 0; i < r.redisConf.MaxRetry; i++ {
		conn := r.RedisConnPool.Get()
		if err = conn.Err(); err != nil {
			continue
		}
		reply, err = conn.Do(cmd, args...)
		if err != nil {
			_ = conn.Close()
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

	for i := 0; i < 10; i++ {
		c, err = newConn(r.redisConf.Nodes, r.redisConf.Password, r.redisConf.DbNo)
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
		redis.DialConnectTimeout(5*time.Second),
		redis.DialReadTimeout(5*time.Second),
		redis.DialDatabase(db),
		redis.DialPassword(passwd))
	if err != nil {
		CubeLog.Fatalf("连接redis:%s 失败:%s", serviceName, err)
		return c, err
	}
	return c, err
}

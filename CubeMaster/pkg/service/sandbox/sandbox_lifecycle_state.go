// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
)

// Wire contract with CLM internal/cubemasterclient: paired with
// ErrorCode_Conflict (130409). Keep both sides and their literal contract
// tests in sync; this message distinguishes stale intent from lock contention.
const pauseSupersededMessage = "auto-pause superseded by lifecycle state change"

var errPauseSuperseded = errors.New(pauseSupersededMessage)

// resumedLifecycleStateTTL protects a completed resume until CLM processes its
// running event. It matches CLM's default StateLockTTL, but is independent of
// that configurable TTL and of the Master lifecycle operation lock lifetime.
const resumedLifecycleStateTTL = 60 * time.Second

func checkPauseLifecycleState(sandboxID, expected string) error {
	r := wrapredis.GetRedis()
	if r == nil || r.RedisConnPool == nil {
		return sandboxlock.ErrRedisUnavailable
	}
	state, err := redis.String(r.Do("GET", rediskey.SandboxLifecycleState(sandboxID)))
	if err != nil && !errors.Is(err, redis.ErrNil) {
		return err
	}
	if state != expected {
		return errPauseSuperseded
	}
	return nil
}

// Called while the Master resume lock is held. The short-lived running marker
// rejects pending auto-pauses until the existing running event refreshes CLM's
// activity timestamp. It uses the same default 60s marker lifetime as CLM.
func markResumedLifecycleState(sandboxID string) error {
	r := wrapredis.GetRedis()
	if r == nil || r.RedisConnPool == nil {
		return sandboxlock.ErrRedisUnavailable
	}
	_, err := r.Do("SET", rediskey.SandboxLifecycleState(sandboxID), "running", "EX", int(resumedLifecycleStateTTL.Seconds()))
	return err
}

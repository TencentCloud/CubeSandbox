// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxlock serializes Master sandbox lifecycle RPCs that must not
// overlap on one sandboxID: pause / resume (including CLM auto-pause /
// auto-resume, which call Master Update) and delete.
//
// Mechanism: SET key NX EX 10, unlock with DEL. No Lua, no renew — a missed
// unlock self-heals when the 10s TTL expires.
package sandboxlock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

const (
	// DefaultTTL is short so a crashed/leaked holder cannot block the sandbox.
	DefaultTTL = 10 * time.Second
	// DefaultRetryInterval is the poll gap while waiting for the lock.
	DefaultRetryInterval = 50 * time.Millisecond
)

var (
	// ErrLockNotAcquired is returned when the wait budget expires before the lock is held.
	ErrLockNotAcquired = errors.New("sandbox lifecycle lock not acquired")
	// ErrRedisUnavailable is returned when the shared Redis pool cannot be used.
	ErrRedisUnavailable = errors.New("redis unavailable for sandbox lifecycle lock")
)

type doer interface {
	Do(cmd string, args ...interface{}) (interface{}, error)
}

// Options configures WithLock.
type Options struct {
	TTL           time.Duration
	RetryInterval time.Duration
	// Value is stored as the lock string (debug only). Empty → "1".
	Value string
}

// WithLock waits for the per-sandbox op lock, runs fn, then DEL the key.
// Covered callers: pause, resume, delete (auto-pause / auto-resume go through
// Master Update and therefore share this lock).
func WithLock(ctx context.Context, sandboxID string, opts Options, fn func(context.Context) error) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("sandboxID is required")
	}
	if fn == nil {
		return errors.New("nil lock callback")
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = DefaultRetryInterval
	}
	val := strings.TrimSpace(opts.Value)
	if val == "" {
		val = "1"
	}

	r := wrapredis.GetRedis()
	if r == nil || r.RedisConnPool == nil {
		return ErrRedisUnavailable
	}
	return withLock(ctx, r, rediskey.LockSandbox(sandboxID), val, opts, fn)
}

func withLock(ctx context.Context, r doer, key, val string, opts Options, fn func(context.Context) error) error {
	if err := acquire(ctx, r, key, val, opts); err != nil {
		return err
	}
	defer func() {
		if _, err := r.Do("DEL", key); err != nil {
			log.G(ctx).Warnf("sandboxlock: DEL %s failed: %v", key, err)
		}
	}()
	return fn(ctx)
}

func acquire(ctx context.Context, r doer, key, val string, opts Options) error {
	ttlSec := int(opts.TTL.Seconds())
	if ttlSec < 1 {
		ttlSec = 1
	}
	ticker := time.NewTicker(opts.RetryInterval)
	defer ticker.Stop()

	for {
		reply, err := r.Do("SET", key, val, "NX", "EX", ttlSec)
		if err != nil {
			return fmt.Errorf("sandboxlock SET NX: %w", err)
		}
		if s, ok := reply.(string); ok && s == "OK" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrLockNotAcquired, ctx.Err())
		case <-ticker.C:
		}
	}
}

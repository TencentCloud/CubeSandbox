// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxlock serializes Master sandbox lifecycle RPCs that must not
// overlap on one sandboxID: pause / resume (including CLM auto-pause /
// auto-resume, which call Master Update) and delete.
//
// Contract: callers MUST hold the lock until the operation reaches a terminal
// Master outcome (success or recorded failure). Do not return from the
// WithLock callback while leaving CREATING/in-flight work for a background
// goroutine — that would allow resume/delete to interleave. Ignore HTTP/client
// cancel inside the callback (context.WithoutCancel) so disconnect cannot
// unlock mid-op.
//
// Mechanism: SET key NX EX <TTL> with a per-acquire random token; unlock with
// Lua (GET+DEL only when the token still matches). A missed unlock self-heals
// when the TTL expires; a late unlock after expiry cannot delete a newer
// holder's lock. Pause / Resume / Delete use lock TTLs aligned with the
// Pause RPC budget (see PauseTTL / LifecycleTTL).
package sandboxlock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

const (
	// DefaultTTL covers short lifecycle ops. Unlock is always via defer in
	// WithLock; TTL is only a crash/leak safety net.
	DefaultTTL = 60 * time.Second
	// LifecycleTTL covers Resume/Delete under the unified 120s budget.
	LifecycleTTL = 120 * time.Second
	// PauseTTL covers Cubelet Update(pause): PauseToSnapshot + in-process
	// keep_tombstone Destroy under pauseCubeletRPCTimeout (120s), plus Master Complete.
	PauseTTL = 180 * time.Second
	// ResumeTTL is an alias of LifecycleTTL for resume callers.
	ResumeTTL = LifecycleTTL
	// DeleteTTL is an alias of LifecycleTTL for delete callers.
	DeleteTTL = LifecycleTTL
	// DefaultRetryInterval is the poll gap while waiting for the lock.
	DefaultRetryInterval = 50 * time.Millisecond

	// unlockScript deletes KEYS[1] only when its value still equals ARGV[1]
	// (this holder's token). Returns 1 if deleted, 0 if ownership was lost.
	unlockScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`
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
	// Value is an optional debug prefix stored with the random lock token
	// (e.g. "pause"). Ownership always uses a per-acquire UUID suffix.
	Value string
}

// WithLock waits for the per-sandbox op lock, runs fn, then token-safe unlock.
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
	token := lockToken(opts.Value)

	r := wrapredis.GetRedis()
	if r == nil || r.RedisConnPool == nil {
		return ErrRedisUnavailable
	}
	return withLock(ctx, r, rediskey.LockSandbox(sandboxID), token, opts, fn)
}

func lockToken(debugPrefix string) string {
	id := uuid.NewString()
	if p := strings.TrimSpace(debugPrefix); p != "" {
		return p + ":" + id
	}
	return id
}

func withLock(ctx context.Context, r doer, key, token string, opts Options, fn func(context.Context) error) error {
	if err := acquire(ctx, r, key, token, opts); err != nil {
		return err
	}
	defer func() {
		if err := unlock(r, key, token); err != nil {
			log.G(ctx).Warnf("sandboxlock: unlock %s failed: %v", key, err)
		}
	}()
	return fn(ctx)
}

func unlock(r doer, key, token string) error {
	_, err := r.Do("EVAL", unlockScript, 1, key, token)
	return err
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

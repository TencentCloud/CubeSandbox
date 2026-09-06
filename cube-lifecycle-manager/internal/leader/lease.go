// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package leader elects the CLM replica allowed to run singleton maintenance
// work. Warm standbys continue serving resume requests and synchronising state.
package leader

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

var errNotOwner = errors.New("leader lease not owned")
var errLeaseExpired = errors.New("local leader lease deadline expired")

// Status is the read-only role view injected into CLM components.
type Status interface {
	IsLeader() bool
	Enabled() bool
}

// Options configures a Lease.
type Options struct {
	Redis         redis.UniversalClient
	Key           string
	Identity      string
	Enabled       bool
	TTL           time.Duration
	RenewInterval time.Duration
	RetryInterval time.Duration
	Log           *zap.Logger
}

// Lease uses SET NX PX to acquire leadership and single-key WATCH transactions
// to renew and release it. No Lua scripts are used.
type Lease struct {
	o          Options
	token      string
	leader     atomic.Bool
	generation atomic.Uint64
	deadline   atomic.Pointer[time.Time]
	enabled    bool
	store      leaseStore
}

// New constructs a lease. When election is disabled, IsLeader always reports
// true so single-instance deployments keep their existing behavior.
func New(o Options) *Lease {
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	token := o.Identity + ":" + uuid.NewString()
	l := &Lease{
		o:       o,
		token:   token,
		enabled: o.Enabled,
		store:   redisLeaseStore{rdb: o.Redis, key: o.Key},
	}
	if !o.Enabled {
		l.leader.Store(true)
		l.generation.Store(1)
	}
	return l
}

func (l *Lease) IsLeader() bool {
	if !l.leader.Load() {
		return false
	}
	deadline := l.deadline.Load()
	if deadline == nil {
		return !l.enabled
	}
	if !time.Now().Before(*deadline) {
		l.leader.CompareAndSwap(true, false)
		l.deadline.Store(nil)
		return false
	}
	return true
}

func (l *Lease) Enabled() bool { return l.enabled }

// Generation increments after every successful acquisition. It allows
// callers to run one-time catch-up before using a newly promoted replica.
func (l *Lease) Generation() uint64 { return l.generation.Load() }

// Role returns the operator-facing role name.
func (l *Lease) Role() string {
	if l.IsLeader() {
		return "leader"
	}
	return "standby"
}

// Run competes for and renews leadership until ctx is cancelled.
func (l *Lease) Run(ctx context.Context) error {
	if !l.enabled {
		<-ctx.Done()
		return ctx.Err()
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		acquired, err := l.acquire(ctx)
		if err != nil {
			l.o.Log.Warn("leader lease acquire failed", zap.Error(err))
			if !wait(ctx, l.o.RetryInterval) {
				return ctx.Err()
			}
			continue
		}
		if !acquired {
			if !wait(ctx, l.o.RetryInterval) {
				return ctx.Err()
			}
			continue
		}

		l.leader.Store(true)
		l.generation.Add(1)
		l.o.Log.Info("leader lease acquired",
			zap.String("identity", l.o.Identity),
			zap.Duration("ttl", l.o.TTL))

		if err := l.hold(ctx); err != nil && !errors.Is(err, context.Canceled) {
			l.o.Log.Warn("leader lease lost", zap.Error(err))
		}
		l.demote()

		if err := ctx.Err(); err != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			if releaseErr := l.release(releaseCtx); releaseErr != nil {
				l.o.Log.Warn("leader lease release failed", zap.Error(releaseErr))
			} else {
				l.o.Log.Info("leader lease released", zap.String("identity", l.o.Identity))
			}
			cancel()
			return err
		}

		if !wait(ctx, l.o.RetryInterval) {
			return ctx.Err()
		}
	}
}

func (l *Lease) acquire(ctx context.Context) (bool, error) {
	started := time.Now()
	opCtx, cancel := l.operationContext(ctx)
	defer cancel()
	acquired, err := l.store.Acquire(opCtx, l.token, l.o.TTL)
	if err != nil || !acquired {
		return acquired, err
	}
	if !l.setDeadline(started) {
		_ = l.store.Release(opCtx, l.token)
		return false, errLeaseExpired
	}
	return true, nil
}

func (l *Lease) hold(ctx context.Context) error {
	ticker := time.NewTicker(l.o.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !l.IsLeader() {
				return errLeaseExpired
			}
			ok, err := l.renew(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return errNotOwner
			}
		}
	}
}

func (l *Lease) renew(ctx context.Context) (bool, error) {
	started := time.Now()
	opCtx, cancel := l.operationContext(ctx)
	defer cancel()
	renewed, err := l.store.Renew(opCtx, l.token, l.o.TTL)
	if err != nil || !renewed {
		return renewed, err
	}
	if !l.setDeadline(started) {
		return false, errLeaseExpired
	}
	return true, nil
}

func (l *Lease) release(ctx context.Context) error {
	return l.store.Release(ctx, l.token)
}

func (l *Lease) demote() {
	l.leader.Store(false)
	l.deadline.Store(nil)
}

func (l *Lease) setDeadline(started time.Time) bool {
	margin := l.o.RenewInterval
	if margin <= 0 || margin >= l.o.TTL {
		margin = l.o.TTL / 3
	}
	deadline := started.Add(l.o.TTL - margin)
	if !time.Now().Before(deadline) {
		l.deadline.Store(nil)
		return false
	}
	l.deadline.Store(&deadline)
	return true
}

func (l *Lease) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := l.o.RenewInterval
	maxTimeout := l.o.TTL / 3
	if timeout <= 0 || (maxTimeout > 0 && timeout > maxTimeout) {
		timeout = maxTimeout
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	return context.WithTimeout(parent, timeout)
}

type leaseStore interface {
	Acquire(ctx context.Context, token string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, token string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, token string) error
}

type redisLeaseStore struct {
	rdb redis.UniversalClient
	key string
}

func (s redisLeaseStore) Acquire(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, s.key, token, ttl).Result()
}

func (s redisLeaseStore) Renew(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	var renewed bool
	err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
		value, err := tx.Get(ctx, s.key).Result()
		if errors.Is(err, redis.Nil) || value != token {
			return errNotOwner
		}
		if err != nil {
			return err
		}

		var expire *redis.BoolCmd
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			expire = pipe.PExpire(ctx, s.key, ttl)
			return nil
		})
		if err != nil {
			return err
		}
		renewed, err = expire.Result()
		return err
	}, s.key)
	if errors.Is(err, errNotOwner) || errors.Is(err, redis.TxFailedErr) {
		return false, nil
	}
	return renewed, err
}

func (s redisLeaseStore) Release(ctx context.Context, token string) error {
	err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
		value, err := tx.Get(ctx, s.key).Result()
		if errors.Is(err, redis.Nil) || value != token {
			return errNotOwner
		}
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, s.key)
			return nil
		})
		return err
	}, s.key)
	if errors.Is(err, errNotOwner) || errors.Is(err, redis.TxFailedErr) {
		return nil
	}
	return err
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

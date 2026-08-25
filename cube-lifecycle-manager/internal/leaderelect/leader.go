// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package leaderelect implements Redis-based active-standby leader election
// for the cube-lifecycle-manager (issue #1211). Multiple CLM replicas run
// against the same Redis; exactly one holds the leader lease at a time and
// runs the stream consumer / sweeper / reconciler loops, while the others
// stay hot standbys (HTTP server only) ready to take over within one lease
// TTL of a failure.
//
// The lease follows the same SETNX + token + Lua idiom as
// CubeMaster/pkg/sandboxlock: acquire via SET key id NX EX ttl, renew via a
// Lua compare-and-expire so a stale holder can never extend a lock it has
// already lost, and release via a Lua compare-and-delete on graceful
// shutdown so the standby promotes immediately instead of waiting out the
// TTL.
package leaderelect

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Config tunes the lease. TTL must comfortably exceed RenewInterval so a
// single missed renewal (Redis blip, GC pause) doesn't cost leadership.
type Config struct {
	// Key is the Redis lock key, e.g.
	// cube:v1:shared:lock:lifecycle_manager:leader (registered in
	// docs/zh/dev/redis-key-spec.md; follows the cube:v1:{scope}:lock:{resource}:{id}
	// naming rule for distributed locks).
	Key string
	// InstanceID uniquely identifies this replica (pod name / hostname). It
	// is the fencing token written into the lock value.
	InstanceID string
	// TTL is the lock's expiry. A crashed leader blocks failover for at most
	// this long.
	TTL time.Duration
	// RenewInterval is the cadence of lease renewal (and of acquisition
	// retries while standby).
	RenewInterval time.Duration
}

func (c Config) validate() error {
	switch {
	case c.Key == "":
		return errors.New("leader key is empty")
	case c.InstanceID == "":
		return errors.New("instance id is empty")
	case c.TTL <= 0:
		return errors.New("leader TTL must be > 0")
	case c.RenewInterval <= 0:
		return errors.New("renew interval must be > 0")
	case c.RenewInterval >= c.TTL:
		return errors.New("renew interval must be < leader TTL")
	}
	return nil
}

// store is the subset of Redis the elector needs. Tests substitute an
// in-memory fake; production uses redisStore over a go-redis client.
type store interface {
	// tryAcquire attempts SET key id NX EX ttl. Returns true when the lease
	// was taken by this instance.
	tryAcquire(ctx context.Context, key, id string, ttl time.Duration) (bool, error)
	// renew extends the TTL only if the lock value still equals id (Lua
	// compare-and-expire). Returns false when the lease is no longer ours.
	renew(ctx context.Context, key, id string, ttl time.Duration) (bool, error)
	// release deletes the key only if the lock value still equals id (Lua
	// compare-and-delete). Best-effort; the TTL is the ultimate backstop.
	release(ctx context.Context, key, id string) error
}

// Callbacks are invoked on leadership transitions.
type Callbacks struct {
	// OnElected runs in its own goroutine each time this instance acquires
	// leadership. The supplied context is cancelled when leadership is lost
	// (or the parent context ends), so leader-only loops should select on
	// ctx.Done(). OnElected must be safe to invoke repeatedly across
	// lose-then-regain cycles.
	OnElected func(ctx context.Context)
	// OnLost runs synchronously after the OnElected context is cancelled and
	// the previous OnElected goroutine has drained (bounded by
	// stintDrainTimeout), so teardown here never races an in-flight leader
	// loop from the demoted stint. Optional.
	OnLost func()
}

// stintDrainTimeout bounds how long loseLeadership waits for the demoted
// stint's OnElected goroutine to return before invoking OnLost. A healthy
// stint drains almost immediately after cancellation (in-flight HTTP pushes
// and stream reads carry the cancelled context); the bound only guards
// against a wedged leader loop blocking re-election forever.
const stintDrainTimeout = 30 * time.Second

// Elector maintains the leader lease. Construct with New, then call Run.
type Elector struct {
	cfg Config
	s   store
	log *zap.Logger

	leader   atomic.Bool
	stepDown chan struct{} // buffered(1); a send requests voluntary loss of leadership

	// cancelLeader cancels the context handed to the current OnElected
	// stint. Only the Run goroutine reads/writes it.
	cancelLeader context.CancelFunc
	// stintDone is closed when the current OnElected goroutine returns; nil
	// between stints. Only the Run goroutine reads/writes it.
	stintDone chan struct{}
}

// New builds an Elector over a go-redis client.
func New(rdb redis.UniversalClient, cfg Config, log *zap.Logger) (*Elector, error) {
	return NewWithStore(redisStore{rdb: rdb}, cfg, log)
}

// NewWithStore builds an Elector over a custom store (tests).
func NewWithStore(s store, cfg Config, log *zap.Logger) (*Elector, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Elector{
		cfg:      cfg,
		s:        s,
		log:      log,
		stepDown: make(chan struct{}, 1),
	}, nil
}

// IsLeader reports whether this instance currently holds the lease. Used by
// the HTTP server's /readyz gate so only the active replica receives resume
// traffic from the Service.
func (e *Elector) IsLeader() bool { return e.leader.Load() }

// StepDown asks the elector to voluntarily release leadership (e.g. because
// a leader-only loop failed and a fresh takeover is safer than limping on).
// The lease is released immediately so a standby can promote; this instance
// then resumes normal acquisition attempts, which typically re-elects it and
// restarts the failed loops.
func (e *Elector) StepDown() {
	select {
	case e.stepDown <- struct{}{}:
	default:
	}
}

// Run drives the acquisition/renewal loop until ctx is cancelled. It returns
// ctx.Err() (nil-adjacent for callers that treat context.Canceled as a clean
// shutdown).
func (e *Elector) Run(ctx context.Context, cb Callbacks) error {
	t := time.NewTicker(e.cfg.RenewInterval)
	defer t.Stop()

	lastRenewOK := time.Now()

	loseLeadership := func(reason string, release bool) {
		if !e.leader.CompareAndSwap(true, false) {
			return
		}
		e.log.Warn("leadership lost",
			zap.String("reason", reason),
			zap.String("instance_id", e.cfg.InstanceID))
		if e.cancelLeader != nil {
			e.cancelLeader()
			e.cancelLeader = nil
		}
		if e.stintDone != nil {
			// Drain the demoted stint before OnLost: teardown (e.g. the
			// registry reset) must not race a leader loop that is still
			// mid-handleEvent, and a re-elected stint must not start while
			// the old one is still writing. Bounded so a wedged loop can't
			// block re-election forever.
			select {
			case <-e.stintDone:
			case <-time.After(stintDrainTimeout):
				e.log.Warn("timed out waiting for leader loops to drain; proceeding with OnLost")
			}
			e.stintDone = nil
		}
		if release {
			// Bounded context independent of ctx: on shutdown ctx is already
			// cancelled, but the release must still reach Redis so the
			// standby promotes without waiting out the TTL.
			relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := e.s.release(relCtx, e.cfg.Key, e.cfg.InstanceID); err != nil {
				e.log.Warn("leader lease release failed; standby waits for TTL",
					zap.String("key", e.cfg.Key), zap.Error(err))
			}
			cancel()
		}
		if cb.OnLost != nil {
			cb.OnLost()
		}
	}
	defer loseLeadership("shutdown", true)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.stepDown:
			loseLeadership("step down requested", true)
			continue
		case <-t.C:
		}

		if !e.leader.Load() {
			ok, err := e.s.tryAcquire(ctx, e.cfg.Key, e.cfg.InstanceID, e.cfg.TTL)
			if err != nil {
				e.log.Warn("leader acquire failed; retrying",
					zap.String("key", e.cfg.Key), zap.Error(err))
				continue
			}
			if !ok {
				continue
			}
			e.leader.Store(true)
			lastRenewOK = time.Now()
			var leaderCtx context.Context
			leaderCtx, e.cancelLeader = context.WithCancel(ctx)
			e.stintDone = make(chan struct{})
			stintDone := e.stintDone
			e.log.Info("acquired leadership",
				zap.String("key", e.cfg.Key),
				zap.String("instance_id", e.cfg.InstanceID))
			if cb.OnElected != nil {
				go func() {
					defer close(stintDone)
					cb.OnElected(leaderCtx)
				}()
			} else {
				close(stintDone)
			}
			continue
		}

		ok, err := e.s.renew(ctx, e.cfg.Key, e.cfg.InstanceID, e.cfg.TTL)
		switch {
		case err == nil && ok:
			lastRenewOK = time.Now()
		case err == nil && !ok:
			// The lease expired and (maybe) moved to a peer. Do NOT release:
			// the key is no longer ours to delete.
			loseLeadership("lease held by another instance", false)
		default:
			// Transport error: tolerate isolated misses — the lease only
			// truly escapes once the TTL elapses without a successful renew.
			if time.Since(lastRenewOK) > e.cfg.TTL {
				loseLeadership("renewals failing past TTL", false)
			} else {
				e.log.Warn("leader renew failed; retrying",
					zap.String("key", e.cfg.Key), zap.Error(err))
			}
		}
	}
}

// redisStore implements store over go-redis.
type redisStore struct {
	rdb redis.UniversalClient
}

func (s redisStore) tryAcquire(ctx context.Context, key, id string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, key, id, ttl).Result()
}

// renewScript extends the lease only when the holder matches. Returns 1 on
// success, 0 when the lease is gone or owned by someone else.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

func (s redisStore) renew(ctx context.Context, key, id string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, s.rdb, []string{key}, id, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// releaseScript deletes the lease only when the holder matches, so a stale
// leader can never delete a lock a peer has since acquired.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

func (s redisStore) release(ctx context.Context, key, id string) error {
	return releaseScript.Run(ctx, s.rdb, []string{key}, id).Err()
}

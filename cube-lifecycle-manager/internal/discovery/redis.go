// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisDiscovery is the production Fleet: it periodically reads the
// registration Hash + heartbeat Sorted Set from Redis and keeps an in-memory
// map of live endpoints.
type RedisDiscovery struct {
	rdb       *redis.Client
	log       *zap.Logger
	ttl       time.Duration
	refresh   time.Duration
	onJoin    func(Endpoint)
	onLeave   func(string)

	mu    sync.RWMutex
	state map[string]*live
}

// Options configures a RedisDiscovery instance.
type Options struct {
	Redis           *redis.Client
	Log             *zap.Logger
	HeartbeatTTL    time.Duration // members with score < now-TTL are considered dead
	RefreshInterval time.Duration // how often to re-scan Redis

	// OnJoin fires exactly once when a proxy first appears in the live set.
	// Used by main to trigger a registry replay (HGETALL meta → push).
	OnJoin func(Endpoint)
	// OnLeave fires when a proxy's heartbeat has aged past TTL.
	OnLeave func(proxyID string)
}

// New builds a RedisDiscovery with sane defaults for zero-valued options.
func New(o Options) *RedisDiscovery {
	if o.HeartbeatTTL <= 0 {
		o.HeartbeatTTL = 15 * time.Second
	}
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = 3 * time.Second
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.OnJoin == nil {
		o.OnJoin = func(Endpoint) {}
	}
	if o.OnLeave == nil {
		o.OnLeave = func(string) {}
	}
	return &RedisDiscovery{
		rdb:     o.Redis,
		log:     o.Log,
		ttl:     o.HeartbeatTTL,
		refresh: o.RefreshInterval,
		onJoin:  o.OnJoin,
		onLeave: o.OnLeave,
		state:   make(map[string]*live),
	}
}

// Snapshot returns the current live endpoint set. Safe for concurrent use.
func (d *RedisDiscovery) Snapshot() []Endpoint {
	return snapshotMap(d.state, &d.mu)
}

// Run drives the refresh loop until ctx is cancelled. It always performs one
// refresh before entering the ticker loop so the fleet is warm as soon as Run
// returns to its caller (once the initial refresh has synced).
func (d *RedisDiscovery) Run(ctx context.Context) error {
	// A first refresh isn't required to succeed — Redis may still be booting
	// alongside CLM in one-click. Just log and let the ticker retry.
	if err := d.refreshOnce(ctx); err != nil {
		d.log.Warn("discovery: initial refresh failed", zap.Error(err))
	}
	t := time.NewTicker(d.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := d.refreshOnce(ctx); err != nil {
				d.log.Warn("discovery: refresh failed", zap.Error(err))
			}
		}
	}
}

// refreshOnce reads the heartbeat set + registry hash, computes join/leave,
// and prunes expired members. It swaps the internal state atomically so
// concurrent Snapshot() readers never observe a torn view.
func (d *RedisDiscovery) refreshOnce(ctx context.Context) error {
	nowMs := time.Now().UnixMilli()
	cutoff := nowMs - d.ttl.Milliseconds()

	// ZRANGEBYSCORE heartbeat (cutoff +inf WITHSCORES
	// The exclusive-lower-bound "(cutoff" wire encoding is the string form
	// "(" + strconv.FormatInt(cutoff, 10); go-redis passes it through.
	rangeArgs := &redis.ZRangeBy{
		Min: "(" + strconv.FormatInt(cutoff, 10),
		Max: "+inf",
	}
	members, err := d.rdb.ZRangeByScoreWithScores(ctx, HeartbeatKey, rangeArgs).Result()
	if err != nil {
		return fmt.Errorf("zrangebyscore %s: %w", HeartbeatKey, err)
	}

	// Collect the IDs we plan to keep, then batch-read their registry rows.
	ids := make([]string, 0, len(members))
	for _, m := range members {
		id, ok := m.Member.(string)
		if !ok || id == "" {
			continue
		}
		ids = append(ids, id)
	}

	// HMGET returns nil for missing fields; we skip those. A missing registry
	// entry with a live heartbeat means the proxy is in the middle of first
	// registration; it'll show up on a subsequent refresh.
	var rows []interface{}
	if len(ids) > 0 {
		got, hmgetErr := d.rdb.HMGet(ctx, RegistryKey, ids...).Result()
		if hmgetErr != nil {
			return fmt.Errorf("hmget %s: %w", RegistryKey, hmgetErr)
		}
		rows = got
	}

	next := make(map[string]*live, len(ids))
	now := time.Now()
	for i, id := range ids {
		raw, ok := rows[i].(string)
		if !ok || raw == "" {
			// heartbeat without a registry entry — skip, retry next refresh
			continue
		}
		ep, decErr := decodeEndpoint(raw)
		if decErr != nil {
			d.log.Warn("discovery: bad registry entry",
				zap.String("proxy_id", id), zap.Error(decErr))
			continue
		}
		// Trust the Hash field name for identity; JSON may lag.
		ep.ProxyID = id
		next[id] = &live{ep: ep, lastSeen: now}
	}

	// Compute transitions against the previous state.
	d.mu.Lock()
	prev := d.state
	joins, leaves := diffJoins(prev, next)
	d.state = next
	d.mu.Unlock()

	for _, id := range joins {
		if l, ok := next[id]; ok {
			d.log.Info("discovery: proxy joined",
				zap.String("proxy_id", id),
				zap.String("admin_url", l.ep.AdminURL))
			d.onJoin(l.ep)
		}
	}
	for _, id := range leaves {
		d.log.Info("discovery: proxy left",
			zap.String("proxy_id", id))
		d.onLeave(id)
	}

	// Best-effort prune of expired heartbeat members + their registry rows.
	// Failures here don't invalidate the refresh (state is already updated),
	// they just delay cleanup until the next tick.
	if err := d.pruneExpired(ctx, cutoff); err != nil {
		d.log.Warn("discovery: prune failed", zap.Error(err))
	}
	return nil
}

// pruneExpired removes heartbeat members whose score is <= cutoff and drops
// the matching registry entries. Callers pass the same cutoff they used for
// the read so the window is consistent.
func (d *RedisDiscovery) pruneExpired(ctx context.Context, cutoff int64) error {
	// ZRANGEBYSCORE ... to collect expired IDs first; we need them to feed
	// HDEL. Doing this before ZREMRANGEBYSCORE keeps the two operations
	// consistent even if a new heartbeat lands in between (worst case: we
	// HDEL a row that got re-registered in the same tick, which the proxy
	// will republish on its next timer tick — self-healing).
	rangeArgs := &redis.ZRangeBy{
		Min: "-inf",
		Max: "(" + strconv.FormatInt(cutoff+1, 10), // inclusive of cutoff
	}
	expired, err := d.rdb.ZRangeByScore(ctx, HeartbeatKey, rangeArgs).Result()
	if err != nil {
		return fmt.Errorf("zrangebyscore expired: %w", err)
	}
	if len(expired) == 0 {
		return nil
	}
	pipe := d.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, HeartbeatKey, "-inf", strconv.FormatInt(cutoff, 10))
	// HDEL variadic accepts fields as []string.
	pipe.HDel(ctx, RegistryKey, expired...)
	_, err = pipe.Exec(ctx)
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("prune pipeline: %w", err)
	}
	return nil
}

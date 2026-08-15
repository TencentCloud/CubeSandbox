// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// Runtime owns the whole in-process webhook subsystem: Redis client, consumer,
// supervisor and cleanup. It exposes the health surface used by /readyz and
// /webhook/healthz.
type Runtime struct {
	rdb        redis.UniversalClient
	consumer   *Consumer
	supervisor *Supervisor
	cleanup    *Cleanup
	backlog    *BacklogCache
	started    atomic.Bool
}

// NewRuntime wires the webhook subsystem from config. Returns nil when
// webhook.enabled=false (callers must check).
func NewRuntime(cfg *config.Config, s *store.Store) (*Runtime, error) {
	w := &cfg.Webhook
	if !w.Enabled {
		return nil, nil
	}
	rdb, err := newRedisClient(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	effectiveLease := w.LeaseDuration
	if twice := 2 * w.HTTPTimeout; twice > effectiveLease {
		effectiveLease = twice
	}
	consumerName := w.ConsumerName
	if consumerName == "" {
		host, _ := os.Hostname()
		consumerName = fmt.Sprintf("%s-%x", host, time.Now().UnixNano()&0xffffffff)
	}
	ds := NewDeliveryStore(s.DB())
	backlog := NewBacklogCache(w.KeepPendingMaxRetryWindow)
	sender := NewSender(w.HTTPTimeout, w.AllowPrivateNetworks)
	consumer := NewConsumer(
		rdb, ds, s,
		w.ConsumerGroup, consumerName,
		w.ConsumerBatchSize,
		w.ReadBlock, w.KeepPendingMaxRetryWindow, 2*effectiveLease,
		w.BacklogWatermark, w.MaxSubscriptionsPerEvent,
		backlog,
	)
	supervisor := NewSupervisor(
		ds, sender, backlog, consumerName,
		effectiveLease, w.KeepPendingMaxRetryWindow, w.RetryPollInterval, w.ShutdownTimeout,
		w.ClaimBatchSize, w.WorkerConcurrency, w.PerSubscriptionConcurrency,
		w.PerSubscriptionBacklogLimit, w.MaxAttempts, w.DeadLetterMode,
	)
	cleanup := NewCleanup(
		ds,
		w.Cleanup.SucceededRetention, w.Cleanup.TerminalFailureRetention,
		w.KeepPendingMaxRetryWindow, w.Cleanup.Interval,
	)
	return &Runtime{
		rdb: rdb, consumer: consumer, supervisor: supervisor,
		cleanup: cleanup, backlog: backlog,
	}, nil
}

// Start launches consumer, supervisor and cleanup loops.
func (r *Runtime) Start(ctx context.Context) {
	if r == nil {
		return
	}
	go r.consumer.Run(ctx)
	r.supervisor.Start(ctx)
	go r.cleanup.Run(ctx)
	r.started.Store(true)
	logging.G(ctx).Info("webhook delivery worker started")
}

// Shutdown stops claiming, waits for in-flight sends (grace window, no
// cancel), then releases stragglers and closes Redis.
func (r *Runtime) Shutdown(ctx context.Context) {
	if r == nil {
		return
	}
	r.supervisor.Shutdown(ctx)
	if r.rdb != nil {
		_ = r.rdb.Close()
	}
}

// Started reports whether the subsystem was started.
func (r *Runtime) Started() bool {
	return r != nil && r.started.Load()
}

// Healthy reports end-to-end readiness: Redis reachable + consumer group
// present + supervisor loop running.
func (r *Runtime) Healthy(ctx context.Context) bool {
	if r == nil || !r.started.Load() {
		return false
	}
	return r.consumer.Healthy(ctx) && r.supervisor.Healthy()
}

// RedisPing is a lightweight reachability probe (used by /webhook/healthz
// even before the full consumer is ready).
func (r *Runtime) RedisPing(ctx context.Context) bool {
	if r == nil || r.rdb == nil {
		return false
	}
	return r.rdb.Ping(ctx).Err() == nil
}

// newRedisClient parses a redis:// URL into go-redis options. Supports
// optional password and db index.
func newRedisClient(rawURL string) (redis.UniversalClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	addr := u.Host
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password := ""
	if u.User != nil {
		password, _ = u.User.Password()
	}
	db := 0
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			db = n
		}
	}
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	}), nil
}

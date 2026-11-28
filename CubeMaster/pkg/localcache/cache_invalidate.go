// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/retry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	invalidatePublishDelay = 100 * time.Millisecond
	invalidateResubscribe  = time.Second
	// invalidateQueueSize absorbs a burst; a full queue means Redis is already
	// slow, so dropping is cheaper than stalling the request and the sandbox lock.
	invalidateQueueSize = 4096
)

var (
	cacheInvalidatePublishTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sandbox_cache_invalidate_publish_total",
		Help: "Sandbox cache invalidation publishes, by result (ok, failed, dropped).",
	}, []string{"result"})
	cacheInvalidateReceivedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sandbox_cache_invalidate_received_total",
		Help: "Sandbox cache invalidation messages applied to the local cache.",
	})
	cacheInvalidateResubscribeTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sandbox_cache_invalidate_resubscribe_total",
		Help: "Sandbox cache invalidation subscriptions lost and retried.",
	})

	// invalidateHealthCheck is the PING period; a missing PONG within
	// invalidateHealthCheck+invalidatePongGrace marks the connection dead.
	invalidateHealthCheck = 30 * time.Second
	invalidatePongGrace   = 10 * time.Second

	invalidateQueue = make(chan string, invalidateQueueSize)

	// redisPublishFn, subscribeConnFn and redisExistsFn are test seams;
	// production goes through the shared wrapredis pool.
	redisPublishFn = func(cmd string, args ...interface{}) (interface{}, error) {
		return wrapredis.GetRedis().Do(cmd, args...)
	}
	subscribeConnFn = func() (redis.Conn, error) {
		return wrapredis.GetRedis().SubscribeConn()
	}
	redisExistsFn = func(keys ...interface{}) (interface{}, error) {
		return wrapredis.GetRedis().Do("EXISTS", keys...)
	}
)

// publishSandboxCacheInvalidate drops the local entry now and queues the hint.
// Queueing keeps a degraded Redis from stalling the caller or the sandbox lock.
func publishSandboxCacheInvalidate(ctx context.Context, sandboxID string) {
	if sandboxID == "" {
		return
	}
	DeleteSandboxCache(sandboxID)
	select {
	case invalidateQueue <- sandboxID:
	default:
		cacheInvalidatePublishTotal.WithLabelValues("dropped").Inc()
		log.G(ctx).Warnf("sandbox cache invalidate queue full, dropped sandbox=%s", sandboxID)
	}
}

// runSandboxCacheInvalidatePublisher drains the queue until ctx ends. Repeats and
// reordering are harmless: an invalidation only deletes a cache entry.
func (l *local) runSandboxCacheInvalidatePublisher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sandboxID := <-invalidateQueue:
			// Without recover a panic here kills the whole process, not just the loop.
			recov.WithRecover(func() {
				publishSandboxCacheInvalidateWithRetry(ctx, sandboxID)
			}, func(panicError interface{}) {
				CubeLog.WithContext(ctx).Errorf("sandbox cache invalidate publish panic:%v", panicError)
			})
		}
	}
}

// publishSandboxCacheInvalidateWithRetry is the blocking publish; it never fails
// the caller because the cache TTL bounds a hint that is never delivered.
func publishSandboxCacheInvalidateWithRetry(ctx context.Context, sandboxID string) {
	bf := &wait.Backoff{Steps: 3, Duration: invalidatePublishDelay, Factor: 2, Jitter: 0.3}
	err := retry.DoWithBackoff(func() error {
		_, doErr := redisPublishFn("PUBLISH", rediskey.SandboxCacheInvalidate(), sandboxID)
		return doErr
	}, bf)
	if err != nil {
		cacheInvalidatePublishTotal.WithLabelValues("failed").Inc()
		log.G(ctx).Errorf("sandbox cache invalidate publish failed, sandbox=%s: %v", sandboxID, err)
		return
	}
	cacheInvalidatePublishTotal.WithLabelValues("ok").Inc()
}

// applySandboxCacheInvalidate drops one entry; safe to call for unknown IDs and
// safe to call twice, so duplicate or replayed messages are harmless.
func applySandboxCacheInvalidate(sandboxID string) {
	if sandboxID == "" {
		return
	}
	DeleteSandboxCache(sandboxID)
	cacheInvalidateReceivedTotal.Inc()
}

// runSandboxCacheInvalidateSubscriber keeps one Pub/Sub connection alive until
// ctx ends. Reconnects are expected on Redis failover and must not be fatal.
func (l *local) runSandboxCacheInvalidateSubscriber(ctx context.Context) {
	for {
		recov.WithRecover(func() {
			if err := l.subscribeSandboxCacheInvalidate(ctx); err != nil && ctx.Err() == nil {
				CubeLog.WithContext(ctx).Errorf("sandbox cache invalidate subscribe: %v", err)
			}
		}, func(panicError interface{}) {
			CubeLog.WithContext(ctx).Errorf("sandbox cache invalidate subscribe panic:%v", panicError)
		})
		if ctx.Err() != nil {
			return
		}
		cacheInvalidateResubscribeTotal.Inc()
		select {
		case <-ctx.Done():
			return
		case <-time.After(invalidateResubscribe):
		}
	}
}

func (l *local) subscribeSandboxCacheInvalidate(ctx context.Context) error {
	conn, err := subscribeConnFn()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	channel := rediskey.SandboxCacheInvalidate()
	psc := redis.PubSubConn{Conn: conn}
	if subErr := psc.Subscribe(channel); subErr != nil {
		return subErr
	}
	stopPing := make(chan struct{})
	pingExited := make(chan struct{})
	go func() {
		defer close(pingExited)
		pingSubscription(ctx, psc, stopPing)
	}()
	// Stop the ping goroutine before Close: redigo does not allow Close to race
	// with an in-flight Send.
	defer func() {
		close(stopPing)
		<-pingExited
	}()

	log.G(ctx).Infof("sandbox cache invalidate subscribed: channel=%s", channel)
	for {
		// A read deadline past the PING period turns a silently dead peer into
		// an error; redigo closes the conn on it, so the caller just redials.
		switch msg := psc.ReceiveWithTimeout(invalidateHealthCheck + invalidatePongGrace).(type) {
		case redis.Message:
			applySandboxCacheInvalidate(string(msg.Data))
		case redis.Subscription:
			if msg.Count == 0 {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errors.New("subscription closed by server")
			}
		case error:
			return msg
		}
	}
}

// pingSubscription probes the connection and unsubscribes on shutdown. redigo
// allows one Send/Flush caller alongside one Receive caller on a connection.
func pingSubscription(ctx context.Context, psc redis.PubSubConn, done <-chan struct{}) {
	ticker := time.NewTicker(invalidateHealthCheck)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			_ = psc.Unsubscribe()
			return
		case <-ticker.C:
			if err := psc.Ping(""); err != nil {
				return
			}
		}
	}
}

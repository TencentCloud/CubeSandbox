// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

func withPublishFn(t *testing.T, fn func(string, ...interface{}) (interface{}, error)) {
	t.Helper()
	orig := redisPublishFn
	t.Cleanup(func() { redisPublishFn = orig })
	redisPublishFn = fn
}

// withFreshInvalidateQueue isolates a test from entries queued elsewhere. It must
// run before any publisher goroutine so the swap is not concurrent with a drain.
func withFreshInvalidateQueue(t *testing.T) chan string {
	t.Helper()
	orig := invalidateQueue
	fresh := make(chan string, invalidateQueueSize)
	invalidateQueue = fresh
	t.Cleanup(func() { invalidateQueue = orig })
	return fresh
}

func droppedCount() float64 {
	return testutil.ToFloat64(cacheInvalidatePublishTotal.WithLabelValues("dropped"))
}

func TestPublishWithRetryRetriesThenSucceeds(t *testing.T) {
	var calls int
	withPublishFn(t, func(cmd string, args ...interface{}) (interface{}, error) {
		assert.Equal(t, "PUBLISH", cmd)
		require.Len(t, args, 2)
		assert.Equal(t, rediskey.SandboxCacheInvalidate(), args[0])
		assert.Equal(t, "sb-1", args[1])
		calls++
		if calls < 3 {
			return nil, errors.New("redis blip")
		}
		return int64(1), nil
	})

	publishSandboxCacheInvalidateWithRetry(context.Background(), "sb-1")
	assert.Equal(t, 3, calls, "two failures must be retried, not surfaced")
}

// TestPublishWithRetryGivesUpQuietly: the proxy map write already committed, so an
// unreachable Redis must degrade to a metric, never an error.
func TestPublishWithRetryGivesUpQuietly(t *testing.T) {
	var calls int
	withPublishFn(t, func(string, ...interface{}) (interface{}, error) {
		calls++
		return nil, errors.New("redis down")
	})

	assert.NotPanics(t, func() { publishSandboxCacheInvalidateWithRetry(context.Background(), "sb-2") })
	assert.Equal(t, 3, calls, "must stop at the retry budget")
}

// TestPublishInvalidateDropsLocalEntryAndQueues: the replica that wrote must stop
// using the old route immediately, without waiting for its own message.
func TestPublishInvalidateDropsLocalEntryAndQueues(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	SetSandboxCache("sb-3", &SandboxCache{SandboxID: "sb-3", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-3") })

	publishSandboxCacheInvalidate(context.Background(), "sb-3")

	assert.Nil(t, GetSandboxCache("sb-3"), "the writer replica must not keep the old route")
	assert.Equal(t, "sb-3", <-queued)
}

func TestPublishInvalidateIgnoresEmptyID(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	publishSandboxCacheInvalidate(context.Background(), "")
	assert.Zero(t, len(queued), "an empty sandbox id must not be queued")
}

// TestPublishInvalidateDropsWhenQueueFull: blocking here would hold the sandbox
// lock for as long as Redis takes to time out, so a full queue must drop instead.
func TestPublishInvalidateDropsWhenQueueFull(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	for i := 0; i < invalidateQueueSize; i++ {
		queued <- "filler"
	}
	SetSandboxCache("sb-full", &SandboxCache{SandboxID: "sb-full", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-full") })

	before := droppedCount()
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishSandboxCacheInvalidate(context.Background(), "sb-full")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a full queue must not block the caller")
	}
	assert.Equal(t, before+1, droppedCount(), "a dropped hint must be alerted")
	assert.Nil(t, GetSandboxCache("sb-full"), "the local entry is dropped even when the hint is")
}

// TestPublisherDrainsQueue moves the publish off the caller: the worker performs
// the retrying publish instead.
func TestPublisherDrainsQueue(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	published := make(chan string, 1)
	withPublishFn(t, func(_ string, args ...interface{}) (interface{}, error) {
		published <- args[1].(string)
		return int64(0), nil
	})
	startPublisher(t)

	queued <- "sb-worker"
	select {
	case got := <-published:
		assert.Equal(t, "sb-worker", got)
	case <-time.After(2 * time.Second):
		t.Fatal("the publisher did not drain the queue")
	}
}

// TestPublisherSurvivesPanic: an unrecovered panic would take the whole process
// down, so one poisoned item must not stop every later invalidation.
func TestPublisherSurvivesPanic(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	published := make(chan string, 1)
	withPublishFn(t, func(_ string, args ...interface{}) (interface{}, error) {
		if args[1].(string) == "sb-boom" {
			panic("redis conf is nil")
		}
		published <- args[1].(string)
		return int64(0), nil
	})
	startPublisher(t)

	queued <- "sb-boom"
	queued <- "sb-after"

	select {
	case got := <-published:
		assert.Equal(t, "sb-after", got, "a panic must not stop the publisher")
	case <-time.After(2 * time.Second):
		t.Fatal("the publisher stopped after a panic")
	}
}

func TestApplyInvalidateRemovesEntry(t *testing.T) {
	SetSandboxCache("sb-4", &SandboxCache{SandboxID: "sb-4", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-4") })

	applySandboxCacheInvalidate("sb-4")
	assert.Nil(t, GetSandboxCache("sb-4"))

	// Duplicate delivery and unknown IDs are both harmless.
	applySandboxCacheInvalidate("sb-4")
	applySandboxCacheInvalidate("sb-unknown")
}

func withShortTTL(t *testing.T) {
	t.Helper()
	cfg := config.GetConfig()
	require.NotNil(t, cfg)
	orig := cfg.Common.SandboxCacheTTL
	t.Cleanup(func() { cfg.Common.SandboxCacheTTL = orig })
	cfg.Common.SandboxCacheTTL = 20 * time.Millisecond
}

// expire waits past the TTL set by withShortTTL.
func expire(t *testing.T) {
	t.Helper()
	time.Sleep(40 * time.Millisecond)
}

// TestSandboxCacheTTLOnlyAffectsFreshness: an expired entry stops counting as a
// fresh route but must still prove the sandbox exists.
func TestSandboxCacheTTLOnlyAffectsFreshness(t *testing.T) {
	withShortTTL(t)
	SetSandboxCache("sb-5", &SandboxCache{SandboxID: "sb-5", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-5") })
	require.NotNil(t, GetFreshSandboxCache("sb-5"))

	time.Sleep(40 * time.Millisecond)
	assert.Nil(t, GetFreshSandboxCache("sb-5"), "a missed invalidation must stop steering routing")
	require.NotNil(t, GetSandboxCache("sb-5"), "existence checks must not lose the sandbox")
	assert.Equal(t, "10.0.0.1", GetSandboxCache("sb-5").HostIP)
}

func TestSetSandboxCacheIfAbsentKeepsResolvedHost(t *testing.T) {
	SetSandboxCache("sb-6", &SandboxCache{SandboxID: "sb-6", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-6") })

	SetSandboxCacheIfAbsent("sb-6", &SandboxCache{SandboxID: "sb-6", HostIP: "10.0.0.9"})
	assert.Equal(t, "10.0.0.1", GetSandboxCache("sb-6").HostIP, "a List response must not clobber a fresh host")

	SetSandboxCacheIfAbsent("sb-7", &SandboxCache{SandboxID: "sb-7", HostIP: "10.0.0.2"})
	t.Cleanup(func() { DeleteSandboxCache("sb-7") })
	assert.Equal(t, "10.0.0.2", GetSandboxCache("sb-7").HostIP)
}

// TestSetSandboxCacheIfAbsentRefreshesExpiredEntry: an expired entry is no longer
// authoritative, so a List response must be allowed to correct it.
func TestSetSandboxCacheIfAbsentRefreshesExpiredEntry(t *testing.T) {
	withShortTTL(t)
	SetSandboxCache("sb-10", &SandboxCache{SandboxID: "sb-10", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-10") })

	time.Sleep(40 * time.Millisecond)
	SetSandboxCacheIfAbsent("sb-10", &SandboxCache{SandboxID: "sb-10", HostIP: "10.0.0.2"})
	assert.Equal(t, "10.0.0.2", GetSandboxCache("sb-10").HostIP)
}

// TestListKnownSandboxIDsKeepsExpired: dropping an expired ID would push every
// full-ID request into a cluster-wide scan once its TTL elapsed.
func TestListKnownSandboxIDsKeepsExpired(t *testing.T) {
	withShortTTL(t)
	SetSandboxCache("sb-8", &SandboxCache{SandboxID: "sb-8", HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache("sb-8") })

	time.Sleep(40 * time.Millisecond)
	assert.Contains(t, ListKnownSandboxIDs(), "sb-8")
}

// TestSetProxyMapQueuesEvenOnWriteError: a reply timeout can follow an applied
// HSET, so a failed write must still invalidate other replicas.
func TestSetProxyMapQueuesEvenOnWriteError(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	p := gomonkey.ApplyPrivateMethod(reflect.TypeOf(l), "setByPassProsyToRedis",
		func(*local, context.Context, string, *types.SandboxProxyMap) error { return errors.New("i/o timeout") })
	t.Cleanup(p.Reset)

	err := SetSandboxProxyMap(context.Background(), &types.SandboxProxyMap{SandboxID: "sb-9", HostIP: "10.0.0.2"})
	require.Error(t, err, "the write error must still reach the caller")
	assert.Equal(t, "sb-9", <-queued)
}

// TestDeleteProxyMapQueuesInvalidation covers the teardown path.
func TestDeleteProxyMapQueuesInvalidation(t *testing.T) {
	queued := withFreshInvalidateQueue(t)
	p := gomonkey.ApplyPrivateMethod(reflect.TypeOf(l), "deleteKeyFromRedis",
		func(*local, context.Context, string) error { return nil })
	t.Cleanup(p.Reset)

	require.NoError(t, DeleteSandboxProxyMap(context.Background(), "sb-11"))
	assert.Equal(t, "sb-11", <-queued)
}

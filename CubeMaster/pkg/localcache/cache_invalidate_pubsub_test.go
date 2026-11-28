// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
)

// withSubscribeDial swaps the subscriber dial seam for the test's lifetime.
func withSubscribeDial(t *testing.T, dial func() (redis.Conn, error)) {
	t.Helper()
	orig := subscribeConnFn
	t.Cleanup(func() { subscribeConnFn = orig })
	subscribeConnFn = dial
}

// startSubscriber runs the subscriber and, on cleanup, waits for it to exit
// before earlier cleanups restore the seams it reads.
func startSubscriber(t *testing.T) (cancel func(), stopped <-chan struct{}) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&local{}).runSandboxCacheInvalidateSubscriber(ctx)
	}()
	t.Cleanup(func() {
		cancelFn()
		<-done
	})
	return cancelFn, done
}

// startPublisher runs the invalidation publisher, which performs the real publish.
func startPublisher(t *testing.T) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&local{}).runSandboxCacheInvalidatePublisher(ctx)
	}()
	t.Cleanup(func() {
		cancelFn()
		<-done
	})
}

// useMiniredis points the subscriber and the publisher at one throwaway
// miniredis so the real dial/subscribe/receive path runs in the test.
func useMiniredis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	withFreshInvalidateQueue(t)
	dial := func() (redis.Conn, error) { return redis.Dial("tcp", server.Addr()) }
	withSubscribeDial(t, dial)
	withPublishFn(t, func(cmd string, args ...interface{}) (interface{}, error) {
		conn, err := dial()
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		return conn.Do(cmd, args...)
	})
	return server
}

func waitSubscribed(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	channel := rediskey.SandboxCacheInvalidate()
	require.Eventually(t, func() bool { return mr.PubSubNumSub(channel)[channel] > 0 },
		3*time.Second, 10*time.Millisecond, "subscriber never registered")
}

// TestInvalidateSubscriptionDropsEntry drives Pub/Sub end to end: another
// replica publishes and the subscriber running here drops its entry.
func TestInvalidateSubscriptionDropsEntry(t *testing.T) {
	server := useMiniredis(t)
	const sandboxID = "sb-pubsub-1"
	SetSandboxCache(sandboxID, &SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache(sandboxID) })

	startSubscriber(t)
	waitSubscribed(t, server)
	require.Equal(t, 1, server.Publish(rediskey.SandboxCacheInvalidate(), sandboxID))

	assert.Eventually(t, func() bool { return GetSandboxCache(sandboxID) == nil },
		3*time.Second, 10*time.Millisecond, "an invalidation message must drop the cached host")
}

// TestPublishThenSubscribeConverges covers the writer side and the reader side
// together, which is the path every proxy map write takes in production.
func TestPublishThenSubscribeConverges(t *testing.T) {
	server := useMiniredis(t)
	const sandboxID = "sb-pubsub-2"
	startSubscriber(t)
	startPublisher(t)
	waitSubscribed(t, server)

	SetSandboxCache(sandboxID, &SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache(sandboxID) })
	publishSandboxCacheInvalidate(context.Background(), sandboxID)

	assert.Eventually(t, func() bool { return GetSandboxCache(sandboxID) == nil },
		3*time.Second, 10*time.Millisecond, "a proxy write must invalidate the local route")
}

// TestSubscriberReconnectsAfterServerRestart keeps a Redis failover from
// silently ending invalidation delivery for the rest of the process lifetime.
func TestSubscriberReconnectsAfterServerRestart(t *testing.T) {
	server := useMiniredis(t)
	const sandboxID = "sb-pubsub-3"
	startSubscriber(t)
	waitSubscribed(t, server)

	server.Close()
	require.NoError(t, server.Restart())
	waitSubscribed(t, server)

	SetSandboxCache(sandboxID, &SandboxCache{SandboxID: sandboxID, HostIP: "10.0.0.1"})
	t.Cleanup(func() { DeleteSandboxCache(sandboxID) })
	server.Publish(rediskey.SandboxCacheInvalidate(), sandboxID)
	assert.Eventually(t, func() bool { return GetSandboxCache(sandboxID) == nil },
		3*time.Second, 10*time.Millisecond, "invalidation must resume after reconnect")
}

// startSilentRedis acks SUBSCRIBE and then never answers again, which is how a
// half-open TCP peer looks after a failover that sent no RST.
func startSilentRedis(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				if _, readErr := r.ReadString('\n'); readErr != nil {
					return
				}
				channel := rediskey.SandboxCacheInvalidate()
				_, _ = fmt.Fprintf(c, "*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(channel), channel)
				_, _ = io.Copy(io.Discard, r)
			}(c)
		}
	}()
	return lis.Addr().String(), &accepted
}

func withFastHealthCheck(t *testing.T) {
	t.Helper()
	origCheck, origGrace := invalidateHealthCheck, invalidatePongGrace
	t.Cleanup(func() { invalidateHealthCheck, invalidatePongGrace = origCheck, origGrace })
	invalidateHealthCheck, invalidatePongGrace = 50*time.Millisecond, 50*time.Millisecond
}

// TestSubscriberDetectsHalfOpenPeer: without a read deadline a silent peer would
// block Receive forever and every invalidation after it would be lost.
func TestSubscriberDetectsHalfOpenPeer(t *testing.T) {
	withFastHealthCheck(t)
	addr, accepted := startSilentRedis(t)
	withSubscribeDial(t, func() (redis.Conn, error) { return redis.Dial("tcp", addr) })
	startSubscriber(t)

	assert.Eventually(t, func() bool { return accepted.Load() >= 2 },
		5*time.Second, 20*time.Millisecond, "a missing PONG must force a reconnect")
}

// TestSubscriberStopsOnContextCancel keeps shutdown from waiting on the next
// message or the health-check deadline.
func TestSubscriberStopsOnContextCancel(t *testing.T) {
	server := useMiniredis(t)
	cancel, stopped := startSubscriber(t)
	waitSubscribed(t, server)

	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not exit after ctx cancel")
	}
}

// fakePubSubConn records whether Close overlaps an in-flight Send, and can make
// Receive time out so the reader exits while a PING is still being written.
type fakePubSubConn struct {
	writeFor     time.Duration
	readTimeout  bool
	inWrite      atomic.Bool
	overlap      atomic.Bool
	unsubscribed chan struct{}
	closed       chan struct{}
	unsubOnce    sync.Once
	closeOnce    sync.Once
}

func newFakePubSubConn(writeFor time.Duration) *fakePubSubConn {
	return &fakePubSubConn{
		writeFor:     writeFor,
		unsubscribed: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *fakePubSubConn) write(cmd string) error {
	c.inWrite.Store(true)
	time.Sleep(c.writeFor)
	c.inWrite.Store(false)
	if strings.EqualFold(cmd, "UNSUBSCRIBE") {
		c.unsubOnce.Do(func() { close(c.unsubscribed) })
	}
	return nil
}

func (c *fakePubSubConn) Close() error {
	if c.inWrite.Load() {
		c.overlap.Store(true)
	}
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakePubSubConn) Err() error { return nil }
func (c *fakePubSubConn) Do(cmd string, _ ...interface{}) (interface{}, error) {
	return nil, c.write(cmd)
}

// DoWithTimeout must exist, or redigo's ConnWithTimeout assertion fails and
// ReceiveWithTimeout returns before any PING is sent.
func (c *fakePubSubConn) DoWithTimeout(_ time.Duration, cmd string, _ ...interface{}) (interface{}, error) {
	return nil, c.write(cmd)
}
func (c *fakePubSubConn) Send(cmd string, _ ...interface{}) error { return c.write(cmd) }
func (c *fakePubSubConn) Flush() error                            { return nil }
func (c *fakePubSubConn) Receive() (interface{}, error) {
	return c.ReceiveWithTimeout(0)
}

func (c *fakePubSubConn) ReceiveWithTimeout(d time.Duration) (interface{}, error) {
	if c.readTimeout && d > 0 {
		select {
		case <-c.unsubscribed:
			return []interface{}{[]byte("unsubscribe"), []byte("chan"), int64(0)}, nil
		case <-time.After(d):
			return nil, errors.New("read timeout")
		}
	}
	select {
	case <-c.unsubscribed:
		return []interface{}{[]byte("unsubscribe"), []byte("chan"), int64(0)}, nil
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

// TestSubscriberClosesOnlyAfterPingStops pins the shutdown order: redigo does not
// allow Close to race an in-flight Send, so the reader must wait for the pinger.
func TestSubscriberClosesOnlyAfterPingStops(t *testing.T) {
	origCheck, origGrace := invalidateHealthCheck, invalidatePongGrace
	t.Cleanup(func() { invalidateHealthCheck, invalidatePongGrace = origCheck, origGrace })
	invalidateHealthCheck, invalidatePongGrace = 20*time.Millisecond, 30*time.Millisecond

	conn := newFakePubSubConn(200 * time.Millisecond)
	conn.readTimeout = true
	withSubscribeDial(t, func() (redis.Conn, error) { return conn, nil })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = (&local{}).subscribeSandboxCacheInvalidate(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber did not exit")
	}
	assert.False(t, conn.overlap.Load(), "Close must not run while a PING is in flight")
}

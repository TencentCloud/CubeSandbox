// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

var testCtx = context.Background()

func testOptions() RedisStoreOptions {
	return RedisStoreOptions{MaxRecords: 100, MaxActive: 4, MaxIdle: 2, OperationTimeout: time.Second, TerminalRetention: time.Hour, WriterRetryWindow: time.Minute}
}
func testTask() DeliveryTask {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return DeliveryTask{Event: &Event{id: uuid.New(), eventType: TypeCreated, occurredAt: now, body: []byte("{ \"unicode\":\"你好\\n\", \"x\": 1 }\n")}, SubscriptionID: "subscription:1", TargetURL: "https://example.test/webhook", CreatedAt: now, NextAttemptAt: now, MaxAttempts: 3}
}
func realConf(t *testing.T) config.RedisConf {
	t.Helper()
	addr := os.Getenv("WEBHOOK_REDIS_ADDR")
	if addr == "" {
		t.Skip("set WEBHOOK_REDIS_ADDR to a dedicated Redis instance")
	}
	return config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_REDIS_PASSWORD"), DbNo: 9}
}
func newTestStore(t *testing.T, o RedisStoreOptions) (*RedisDeliveryStore, config.RedisConf) {
	t.Helper()
	conf := realConf(t)
	s, err := NewRedisDeliveryStore(conf, o)
	require.NoError(t, err)
	prefix := "p2-test:" + uuid.NewString() + ":"
	for i, k := range s.keys {
		s.keys[i] = prefix + k.(string)
	}
	t.Cleanup(func() {
		c, err := redis.Dial("tcp", conf.Nodes, redis.DialPassword(conf.Password), redis.DialDatabase(conf.DbNo))
		if err == nil {
			_, err = c.Do("DEL", s.keys...)
			require.NoError(t, err)
			c.Close()
		}
		require.NoError(t, s.Close())
	})
	return s, conf
}
func claimOne(t *testing.T, s DeliveryStore, now time.Time) ClaimedTask {
	t.Helper()
	v, err := s.Claim(testCtx, now, 1, time.Second)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}
func record(t *testing.T, s *RedisDeliveryStore, key string) deliveryRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(testCtx, time.Second)
	defer cancel()
	c, err := s.pool.GetContext(ctx)
	require.NoError(t, err)
	defer c.Close()
	raw, err := redis.Bytes(c.Do("HGET", s.keys[0], key))
	require.NoError(t, err)
	var r deliveryRecord
	require.NoError(t, json.Unmarshal(raw, &r))
	return r
}
func command(t *testing.T, s *RedisDeliveryStore, name string, args ...interface{}) interface{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(testCtx, time.Second)
	defer cancel()
	c, err := s.pool.GetContext(ctx)
	require.NoError(t, err)
	defer c.Close()
	v, err := redis.DoContext(c, ctx, name, args...)
	require.NoError(t, err)
	return v
}

// The same behavior contract can be used for another backend without making a
// second production implementation. Redis-specific corruption tests follow it.
func deliveryBehavior(t *testing.T, factory func(*testing.T) DeliveryStore) {
	t.Run("immutable idempotency and terminal states", func(t *testing.T) {
		for _, state := range []DeliveryState{DeliverySucceeded, DeliveryFailed, DeliveryCancelled} {
			s := factory(t)
			task := testTask()
			original := task.Event.Body()
			require.NoError(t, s.Put(testCtx, task))
			require.NoError(t, s.Put(testCtx, task))
			task.Event.body[0] = '!'
			task.Event.body = original
			c := claimOne(t, s, task.NextAttemptAt)
			require.Equal(t, original, c.Task.Event.Body())
			require.Equal(t, 1, c.Attempts)
			require.NoError(t, s.Put(testCtx, task))
			bad := task
			bad.TargetURL += "/changed"
			require.ErrorIs(t, s.Put(testCtx, bad), ErrDeliveryConflict)
			bad = task
			bad.MaxAttempts++
			require.ErrorIs(t, s.Put(testCtx, bad), ErrDeliveryConflict)
			bad = task
			ec := *task.Event
			ec.body = []byte("different")
			bad.Event = &ec
			require.ErrorIs(t, s.Put(testCtx, bad), ErrDeliveryConflict)
			c.Task.Event.body[0] = '!'
			require.NoError(t, s.Complete(testCtx, c.Receipt, TerminalResult{State: state}))
			require.NoError(t, s.Put(testCtx, task))
			v, err := s.Claim(testCtx, task.NextAttemptAt.Add(time.Minute), 1, time.Second)
			require.NoError(t, err)
			require.Empty(t, v)
		}
	})
	t.Run("schedule takeover and budget", func(t *testing.T) {
		s := factory(t)
		task := testTask()
		require.NoError(t, s.Put(testCtx, task))
		v, err := s.Claim(testCtx, task.NextAttemptAt.Add(-time.Millisecond), 1, time.Second)
		require.NoError(t, err)
		require.Empty(t, v)
		first := claimOne(t, s, task.NextAttemptAt)
		second := claimOne(t, s, first.LeaseUntil)
		require.Equal(t, 2, second.Attempts)
		require.ErrorIs(t, s.Complete(testCtx, first.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStaleReceipt)
		require.ErrorIs(t, s.Retry(testCtx, first.Receipt, first.LeaseUntil, FailureSummary{}), ErrStaleReceipt)
		next := second.LeaseUntil.Add(time.Hour)
		require.NoError(t, s.Retry(testCtx, second.Receipt, next, FailureSummary{Code: "timeout"}))
		v, err = s.Claim(testCtx, next.Add(-time.Millisecond), 1, time.Second)
		require.NoError(t, err)
		require.Empty(t, v)
		last := claimOne(t, s, next)
		require.Equal(t, 3, last.Attempts)
		v, err = s.Claim(testCtx, last.LeaseUntil, 1, time.Second)
		require.NoError(t, err)
		require.Empty(t, v)
		require.ErrorIs(t, s.Complete(testCtx, last.Receipt, TerminalResult{State: DeliverySucceeded}), ErrStaleReceipt)
		require.NoError(t, s.Put(testCtx, task))
		v, err = s.Claim(testCtx, last.LeaseUntil.Add(time.Hour), 1, time.Second)
		require.NoError(t, err)
		require.Empty(t, v)
	})
	t.Run("last retry and multiple subscriptions", func(t *testing.T) {
		s := factory(t)
		a := testTask()
		a.MaxAttempts = 1
		b := a
		b.SubscriptionID = "other:target"
		require.NoError(t, s.Put(testCtx, a))
		require.NoError(t, s.Put(testCtx, b))
		v, err := s.Claim(testCtx, a.NextAttemptAt, 2, time.Second)
		require.NoError(t, err)
		require.Len(t, v, 2)
		for _, c := range v {
			require.NoError(t, s.Retry(testCtx, c.Receipt, a.NextAttemptAt, FailureSummary{HTTPStatus: 503}))
			require.ErrorIs(t, s.Retry(testCtx, c.Receipt, a.NextAttemptAt, FailureSummary{}), ErrStaleReceipt)
		}
		v, err = s.Claim(testCtx, a.NextAttemptAt.Add(time.Hour), 2, time.Second)
		require.NoError(t, err)
		require.Empty(t, v)
	})
}
func TestRedisDeliveryBehavior(t *testing.T) {
	deliveryBehavior(t, func(t *testing.T) DeliveryStore { s, _ := newTestStore(t, testOptions()); return s })
}

func TestRedisCapacityPruneABA(t *testing.T) {
	o := testOptions()
	o.MaxRecords = 2
	s, _ := newTestStore(t, o)
	a, b := testTask(), testTask()
	require.NoError(t, s.Put(testCtx, a))
	require.NoError(t, s.Put(testCtx, b))
	require.ErrorIs(t, s.Put(testCtx, testTask()), ErrDeliveryCapacity)
	require.NoError(t, s.Put(testCtx, a))
	v, err := s.Claim(testCtx, time.Now().Add(time.Second), 2, time.Second)
	require.NoError(t, err)
	require.Len(t, v, 2)
	old := v[0]
	require.NoError(t, s.Complete(testCtx, old.Receipt, TerminalResult{State: DeliverySucceeded}))
	r := record(t, s, old.Receipt.key)
	n, err := s.Prune(testCtx, time.UnixMilli(r.Finished).Add(o.TerminalRetention-time.Millisecond), 32)
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = s.Prune(testCtx, time.UnixMilli(r.Finished).Add(o.TerminalRetention), 32)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, s.Put(testCtx, old.Task))
	fresh := claimOne(t, s, time.Now())
	require.Equal(t, 1, fresh.Attempts)
	require.NotEqual(t, old.Receipt.token, fresh.Receipt.token)
	require.ErrorIs(t, s.Complete(testCtx, old.Receipt, TerminalResult{State: DeliveryFailed}), ErrStaleReceipt)
	require.ErrorIs(t, s.Retry(testCtx, old.Receipt, time.Now(), FailureSummary{}), ErrStaleReceipt)
	require.Equal(t, int64(2), command(t, s, "HLEN", s.keys[0]))
	require.Equal(t, "inflight", record(t, s, v[1].Receipt.key).State)
}
func TestRedisConcurrentOwnershipAndCapacity(t *testing.T) {
	o := testOptions()
	o.MaxRecords = 5
	s, conf := newTestStore(t, o)
	other, err := NewRedisDeliveryStore(conf, o)
	require.NoError(t, err)
	other.keys = s.keys
	defer other.Close()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- s.Put(testCtx, testTask()) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, ErrDeliveryCapacity)
		}
	}
	require.Equal(t, 5, success)
	start = make(chan struct{})
	results := make(chan []ClaimedTask, 2)
	for _, store := range []*RedisDeliveryStore{s, other} {
		wg.Add(1)
		go func(st *RedisDeliveryStore) {
			defer wg.Done()
			<-start
			v, e := st.Claim(testCtx, time.Now().Add(time.Second), 5, time.Second)
			require.NoError(t, e)
			results <- v
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for v := range results {
		for _, c := range v {
			require.False(t, seen[c.Receipt.key])
			seen[c.Receipt.key] = true
		}
	}
	require.Len(t, seen, 5)
}
func TestRedisBoundedExhaustionAndCorruption(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	for i := 0; i < 4; i++ {
		task := testTask()
		task.MaxAttempts = 1
		require.NoError(t, s.Put(testCtx, task))
	}
	now := time.Now().Add(time.Second)
	v, err := s.Claim(testCtx, now, 4, time.Second)
	require.NoError(t, err)
	require.Len(t, v, 4)
	got, err := s.Claim(testCtx, now.Add(time.Second), 2, time.Second)
	require.NoError(t, err)
	require.Empty(t, got)
	require.Equal(t, int64(2), command(t, s, "ZCARD", s.keys[1]))
	// A corrupt second candidate must not mutate the first candidate.
	keys, err := redis.Strings(command(t, s, "ZRANGE", s.keys[1], 0, -1), nil)
	require.NoError(t, err)
	before := record(t, s, keys[0])
	command(t, s, "HSET", s.keys[0], keys[1], "{broken")
	_, err = s.Claim(testCtx, now.Add(time.Second), 2, time.Second)
	require.ErrorIs(t, err, ErrStoreCorrupt)
	require.Equal(t, before, record(t, s, keys[0]))
	command(t, s, "DEL", s.keys[2])
	command(t, s, "SET", s.keys[2], "wrong type")
	require.ErrorIs(t, s.Put(testCtx, testTask()), ErrStoreCorrupt)
	require.Equal(t, int64(4), command(t, s, "HLEN", s.keys[0]))
}
func TestStoreValidation(t *testing.T) {
	conf := config.RedisConf{Nodes: "127.0.0.1:1"}
	o := testOptions()
	o.TerminalRetention = time.Second
	_, err := NewRedisDeliveryStore(conf, o)
	require.ErrorIs(t, err, ErrInvalidDelivery)
	s, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	defer s.Close()
	task := testTask()
	task.Event.body = make([]byte, MaxDeliveryBodyBytes+1)
	require.ErrorIs(t, s.Put(testCtx, task), ErrInvalidDelivery)
	for _, limit := range []int{0, 33} {
		_, err = s.Claim(testCtx, time.Now(), limit, time.Second)
		require.ErrorIs(t, err, ErrInvalidDelivery)
	}
	for _, lease := range []time.Duration{0, time.Microsecond} {
		_, err = s.Claim(testCtx, time.Now(), 1, lease)
		require.ErrorIs(t, err, ErrInvalidDelivery)
	}
	_, err = s.Claim(testCtx, time.UnixMilli(maxStoreMillis), 1, time.Second)
	require.ErrorIs(t, err, ErrInvalidDelivery)
	require.ErrorIs(t, s.Complete(testCtx, Receipt{}, TerminalResult{State: DeliverySucceeded}), ErrInvalidDelivery)
	require.False(t, validSummary(FailureSummary{Message: strings.Repeat("x", 1025)}))
	require.False(t, validSummary(FailureSummary{HTTPStatus: 600}))
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	require.ErrorIs(t, s.Put(ctx, testTask()), context.Canceled)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
}

func TestRedisTakeoverRace(t *testing.T) {
	s, conf := newTestStore(t, testOptions())
	other, err := NewRedisDeliveryStore(conf, testOptions())
	require.NoError(t, err)
	defer other.Close()
	other.keys = s.keys
	for i := 0; i < 12; i++ {
		task := testTask()
		require.NoError(t, s.Put(testCtx, task))
		old := claimOne(t, s, task.NextAttemptAt)
		start := make(chan struct{})
		done := make(chan error, 1)
		go func() { <-start; done <- s.Complete(testCtx, old.Receipt, TerminalResult{State: DeliverySucceeded}) }()
		close(start)
		got, err := other.Claim(testCtx, old.LeaseUntil, 1, time.Second)
		require.NoError(t, err)
		completed := <-done
		if completed == nil {
			require.Empty(t, got)
			require.Equal(t, "succeeded", record(t, s, old.Receipt.key).State)
		} else {
			require.ErrorIs(t, completed, ErrStaleReceipt)
			require.Len(t, got, 1)
			require.Equal(t, 2, got[0].Attempts)
			require.NoError(t, other.Complete(testCtx, got[0].Receipt, TerminalResult{State: DeliverySucceeded}))
		}
	}
	// Expiry alone does not invalidate ownership.
	task := testTask()
	task.NextAttemptAt = time.Now().Add(-time.Hour)
	require.NoError(t, s.Put(testCtx, task))
	old := claimOne(t, s, task.NextAttemptAt)
	require.NoError(t, s.Complete(testCtx, old.Receipt, TerminalResult{State: DeliverySucceeded}))
}
func TestRedisPruneRejectsActiveAndBadBatch(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	task := testTask()
	require.NoError(t, s.Put(testCtx, task))
	first := claimOne(t, s, task.NextAttemptAt)
	require.NoError(t, s.Complete(testCtx, first.Receipt, TerminalResult{State: DeliverySucceeded}))
	second := testTask()
	require.NoError(t, s.Put(testCtx, second))
	active := claimOne(t, s, second.NextAttemptAt)
	score := time.Now().Add(time.Hour).UnixMilli()
	command(t, s, "ZADD", s.keys[2], score, first.Receipt.key, score+1, active.Receipt.key)
	_, err := s.Prune(testCtx, time.Now().Add(2*time.Hour), 2)
	require.ErrorIs(t, err, ErrStoreCorrupt)
	require.Equal(t, int64(2), command(t, s, "HLEN", s.keys[0]))
	require.Equal(t, "inflight", record(t, s, active.Receipt.key).State)
}
func TestRedisSizeAndScriptValidation(t *testing.T) {
	s, _ := newTestStore(t, testOptions())
	task := testTask()
	task.Event.body = []byte(strings.Repeat("\xff", MaxDeliveryBodyBytes))
	task.TargetURL = strings.Repeat("u", MaxDeliveryURLBytes)
	task.SubscriptionID = strings.Repeat("s", MaxSubscriptionIDBytes)
	require.NoError(t, s.Put(testCtx, task))
	task.Event.body[0] = '!'
	c := claimOne(t, s, task.NextAttemptAt)
	require.Equal(t, byte(255), c.Task.Event.body[0])
	require.Len(t, c.Task.Event.body, MaxDeliveryBodyBytes)
	require.NoError(t, s.Retry(testCtx, c.Receipt, time.Now(), FailureSummary{Code: strings.Repeat("c", 64), Message: strings.Repeat("\x00", MaxFailureMessageBytes), HTTPStatus: 599}))
	c = claimOne(t, s, time.Now())
	require.NoError(t, s.Complete(testCtx, c.Receipt, TerminalResult{State: DeliverySucceeded, Summary: FailureSummary{Message: strings.Repeat("\x00", MaxFailureMessageBytes)}}))
	raw, err := redis.Bytes(command(t, s, "HGET", s.keys[0], c.Receipt.key), nil)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), MaxDeliveryRecordBytes)
	_, err = s.run(testCtx, "put", "bad-key", "not-json", 3, time.Now().UnixMilli(), 100)
	require.ErrorIs(t, err, ErrStoreCorrupt)
	_, err = s.run(testCtx, "claim", time.Now().UnixMilli(), -1, 1000, "token", 1000)
	require.Error(t, err)
	require.Equal(t, int64(1), command(t, s, "HLEN", s.keys[0]))
	// A full return batch is bounded by MaxDeliveryBatch * MaxDeliveryRecordBytes.
	for i := 0; i < MaxDeliveryBatch; i++ {
		task := testTask()
		task.Event.body = make([]byte, MaxDeliveryBodyBytes)
		require.NoError(t, s.Put(testCtx, task))
	}
	batch, err := s.Claim(testCtx, time.Now(), MaxDeliveryBatch, time.Second)
	require.NoError(t, err)
	require.Len(t, batch, MaxDeliveryBatch)
}

func TestDeliveryArgumentValidation(t *testing.T) {
	s, err := NewRedisDeliveryStore(config.RedisConf{Nodes: "127.0.0.1:1"}, testOptions())
	require.NoError(t, err)
	defer s.Close()
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	cases := []func(*DeliveryTask){
		func(v *DeliveryTask) { v.Event = nil }, func(v *DeliveryTask) { v.Event.id = uuid.Nil }, func(v *DeliveryTask) { v.Event.eventType = "unknown" },
		func(v *DeliveryTask) { v.Event.occurredAt = time.Time{} }, func(v *DeliveryTask) { v.Event.body = nil }, func(v *DeliveryTask) { v.SubscriptionID = " " },
		func(v *DeliveryTask) { v.SubscriptionID = strings.Repeat("x", MaxSubscriptionIDBytes+1) }, func(v *DeliveryTask) { v.TargetURL = "" },
		func(v *DeliveryTask) { v.TargetURL = strings.Repeat("x", MaxDeliveryURLBytes+1) }, func(v *DeliveryTask) { v.CreatedAt = time.Time{} },
		func(v *DeliveryTask) { v.NextAttemptAt = time.UnixMilli(-1) }, func(v *DeliveryTask) { v.MaxAttempts = 0 }, func(v *DeliveryTask) { v.MaxAttempts = maxDeliveryAttempts + 1 },
	}
	for i, modify := range cases {
		v := testTask()
		modify(&v)
		require.ErrorIs(t, s.Put(ctx, v), ErrInvalidDelivery, "case %d", i)
	}
	require.ErrorIs(t, s.Put(ctx, testTask()), context.Canceled)
	for _, modify := range []func(*RedisStoreOptions){
		func(v *RedisStoreOptions) { v.MaxRecords = 0 }, func(v *RedisStoreOptions) { v.MaxActive = 0 }, func(v *RedisStoreOptions) { v.MaxIdle = v.MaxActive + 1 },
		func(v *RedisStoreOptions) { v.OperationTimeout = 0 }, func(v *RedisStoreOptions) { v.WriterRetryWindow = 0 }, func(v *RedisStoreOptions) { v.TerminalRetention = time.Hour + time.Nanosecond },
	} {
		o := testOptions()
		modify(&o)
		_, err := NewRedisDeliveryStore(config.RedisConf{Nodes: "unused"}, o)
		require.ErrorIs(t, err, ErrInvalidDelivery)
	}
}

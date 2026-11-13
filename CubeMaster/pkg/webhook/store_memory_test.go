// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var testCtx = context.Background()

func testTask() DeliveryTask {
	now := time.Now().UTC()
	return DeliveryTask{Event: &Event{id: uuid.New(), eventType: TypeCreated, occurredAt: now, body: []byte(`{"data":"exact / bytes"}`)}, SubscriptionID: "subscription", TargetURL: "https://example.test/hook?secret=private", CreatedAt: now, NextAttemptAt: now, MaxAttempts: 3}
}
func memoryStore(t *testing.T, capacity int) *MemoryDeliveryStore {
	t.Helper()
	s, err := NewMemoryDeliveryStore(capacity)
	require.NoError(t, err)
	return s
}
func claimOne(t *testing.T, s DeliveryStore, now time.Time) ClaimedTask {
	t.Helper()
	v, err := s.Claim(testCtx, now)
	require.NoError(t, err)
	require.NotNil(t, v)
	return *v
}
func TestMemoryCapacityRetryAndTerminalRelease(t *testing.T) {
	for _, state := range []DeliveryState{DeliverySucceeded, DeliveryFailed, DeliveryCancelled} {
		t.Run(string(state), func(t *testing.T) {
			s := memoryStore(t, 1)
			task := testTask()
			require.NoError(t, s.Put(testCtx, task))
			require.ErrorIs(t, s.Put(testCtx, testTask()), ErrDeliveryCapacity)
			early, err := s.Claim(testCtx, task.NextAttemptAt.Add(-time.Nanosecond))
			require.NoError(t, err)
			require.Nil(t, early)
			first := claimOne(t, s, task.NextAttemptAt)
			require.Equal(t, 1, first.Attempts)
			require.ErrorIs(t, s.Put(testCtx, testTask()), ErrDeliveryCapacity, "inflight counts too")
			next := task.NextAttemptAt.Add(time.Hour)
			require.NoError(t, s.Retry(testCtx, first.Receipt, next, FailureSummary{HTTPStatus: 503}))
			require.ErrorIs(t, s.Complete(testCtx, first.Receipt, TerminalResult{State: state}), ErrStaleReceipt)
			early, err = s.Claim(testCtx, next.Add(-time.Nanosecond))
			require.NoError(t, err)
			require.Nil(t, early)
			second := claimOne(t, s, next)
			require.ErrorIs(t, s.Complete(testCtx, first.Receipt, TerminalResult{State: state}), ErrStaleReceipt)
			require.Equal(t, 2, second.Attempts)
			require.NoError(t, s.Complete(testCtx, second.Receipt, TerminalResult{State: state}))
			require.Zero(t, s.size())
			require.NoError(t, s.Put(testCtx, task))
			current := claimOne(t, s, task.NextAttemptAt)
			require.ErrorIs(t, s.Complete(testCtx, second.Receipt, TerminalResult{State: state}), ErrStaleReceipt)
			require.NoError(t, s.Complete(testCtx, current.Receipt, TerminalResult{State: state}))
		})
	}
	s := memoryStore(t, 1)
	task := testTask()
	task.MaxAttempts = 1
	require.NoError(t, s.Put(testCtx, task))
	last := claimOne(t, s, task.NextAttemptAt)
	require.NoError(t, s.Retry(testCtx, last.Receipt, time.Now(), FailureSummary{}))
	require.Zero(t, s.size())
	require.NoError(t, s.Put(testCtx, testTask()))
}
func TestMemoryImmutableSnapshotAndFanoutIdentity(t *testing.T) {
	s := memoryStore(t, 2)
	task := testTask()
	original := cloneTask(task)
	require.NoError(t, s.Put(testCtx, task))
	require.NoError(t, s.Put(testCtx, task))
	for _, alter := range []func(*DeliveryTask){
		func(t *DeliveryTask) { t.TargetURL += "/changed" },
		func(t *DeliveryTask) { t.Event.body[0] = '!' },
		func(t *DeliveryTask) { t.MaxAttempts++ },
		func(t *DeliveryTask) { t.NextAttemptAt = t.NextAttemptAt.Add(time.Second) },
	} {
		bad := cloneTask(original)
		alter(&bad)
		require.ErrorIs(t, s.Put(testCtx, bad), ErrDeliveryConflict)
	}
	task.Event.body[0] = '!'
	task.TargetURL = "changed"
	first := claimOne(t, s, original.NextAttemptAt)
	require.Equal(t, original.Event.Body(), first.Task.Event.Body())
	require.Equal(t, original.TargetURL, first.Task.TargetURL)
	first.Task.Event.body[0] = '!'
	require.NoError(t, s.Retry(testCtx, first.Receipt, original.NextAttemptAt, FailureSummary{}))
	second := claimOne(t, s, original.NextAttemptAt)
	require.Equal(t, original.Event.Body(), second.Task.Event.Body())
	other := cloneTask(original)
	other.SubscriptionID = "another"
	require.NoError(t, s.Put(testCtx, other))
	require.Equal(t, 2, s.size())
	next := claimOne(t, s, other.NextAttemptAt)
	require.Equal(t, other.SubscriptionID, next.Task.SubscriptionID)
}
func TestMemoryConcurrentCapacityAndClaim(t *testing.T) {
	s := memoryStore(t, 8)
	var wg sync.WaitGroup
	accepted := make(chan DeliveryTask, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := testTask()
			err := s.Put(testCtx, task)
			if err == nil {
				accepted <- task
			} else {
				require.ErrorIs(t, err, ErrDeliveryCapacity)
			}
		}()
	}
	wg.Wait()
	close(accepted)
	require.Len(t, accepted, 8)
	require.Equal(t, 8, s.size())
	claims := make(chan ClaimedTask, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Claim(testCtx, time.Now().Add(time.Second))
			require.NoError(t, err)
			if v != nil {
				claims <- *v
			}
		}()
	}
	wg.Wait()
	close(claims)
	require.Len(t, claims, 8)
	seen := map[uuid.UUID]bool{}
	for v := range claims {
		require.False(t, seen[v.Task.Event.ID()])
		seen[v.Task.Event.ID()] = true
		require.NoError(t, s.Complete(testCtx, v.Receipt, TerminalResult{State: DeliverySucceeded}))
	}
	require.Zero(t, s.size())
}
func TestMemoryValidationAndCancellation(t *testing.T) {
	_, err := NewMemoryDeliveryStore(0)
	require.ErrorIs(t, err, ErrInvalidDelivery)
	s := memoryStore(t, 1)
	for _, alter := range []func(*DeliveryTask){
		func(t *DeliveryTask) { t.Event = nil }, func(t *DeliveryTask) { t.Event.id = uuid.Nil }, func(t *DeliveryTask) { t.Event.eventType = "wrong" },
		func(t *DeliveryTask) { t.Event.body = nil }, func(t *DeliveryTask) { t.Event.body = make([]byte, MaxDeliveryBodyBytes+1) },
		func(t *DeliveryTask) { t.TargetURL = strings.Repeat("x", MaxDeliveryURLBytes+1) }, func(t *DeliveryTask) { t.SubscriptionID = " " },
		func(t *DeliveryTask) { t.CreatedAt = time.Time{} }, func(t *DeliveryTask) { t.NextAttemptAt = time.Time{} }, func(t *DeliveryTask) { t.MaxAttempts = 0 },
	} {
		task := testTask()
		alter(&task)
		require.ErrorIs(t, s.Put(testCtx, task), ErrInvalidDelivery)
	}
	ctx, cancel := context.WithCancel(testCtx)
	cancel()
	task := testTask()
	require.ErrorIs(t, s.Put(ctx, task), context.Canceled)
	require.Zero(t, s.size())
	require.NoError(t, s.Put(testCtx, task))
	v, err := s.Claim(ctx, time.Now())
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, v)
	claim := claimOne(t, s, time.Now())
	require.ErrorIs(t, s.Complete(testCtx, claim.Receipt, TerminalResult{State: "wrong"}), ErrInvalidDelivery)
	require.ErrorIs(t, s.Retry(testCtx, claim.Receipt, time.Time{}, FailureSummary{}), ErrInvalidDelivery)
	require.ErrorIs(t, s.Retry(testCtx, claim.Receipt, time.Now(), FailureSummary{HTTPStatus: 999}), ErrInvalidDelivery)
	require.ErrorIs(t, s.Complete(ctx, claim.Receipt, TerminalResult{State: DeliveryCancelled}), context.Canceled)
	require.NoError(t, s.Complete(testCtx, claim.Receipt, TerminalResult{State: DeliveryCancelled}))
	require.Zero(t, s.size())
}

func TestMemoryConcurrentClaimRetryComplete(t *testing.T) {
	s := memoryStore(t, 16)
	for i := 0; i < 16; i++ {
		require.NoError(t, s.Put(testCtx, testTask()))
	}
	ctx, cancel := context.WithTimeout(testCtx, 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var claims atomic.Int32
	var mu sync.Mutex
	sending := map[uuid.UUID]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task, err := s.Claim(ctx, time.Now())
				require.NoError(t, err)
				if task == nil {
					if s.size() == 0 {
						return
					}
					runtime.Gosched()
					continue
				}
				mu.Lock()
				alreadySending := sending[task.Task.Event.ID()]
				sending[task.Task.Event.ID()] = true
				mu.Unlock()
				require.False(t, alreadySending, "same delivery claimed concurrently")
				runtime.Gosched()
				mu.Lock()
				delete(sending, task.Task.Event.ID())
				mu.Unlock()
				claims.Add(1)
				if task.Attempts < task.Task.MaxAttempts {
					require.NoError(t, s.Retry(ctx, task.Receipt, time.Now(), FailureSummary{HTTPStatus: 503}))
				} else {
					require.NoError(t, s.Complete(ctx, task.Receipt, TerminalResult{State: DeliveryFailed}))
				}
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 48, claims.Load())
	require.Zero(t, s.size())
}

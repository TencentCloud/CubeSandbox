// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func externalTask(t *testing.T) webhook.DeliveryTask {
	t.Helper()
	now := time.Now().UTC()
	return webhook.DeliveryTask{Event: encodedEvent(t), SubscriptionID: "bus", Target: "test-bus/topic-a", CreatedAt: now, NextAttemptAt: now, MaxAttempts: 2}
}

func TestMemoryReceiptOwnershipAndLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := webhook.NewMemoryDeliveryStore(1)
	require.NoError(t, err)
	foreign, err := webhook.NewMemoryDeliveryStore(1)
	require.NoError(t, err)
	task := externalTask(t)
	require.NoError(t, s.Put(ctx, task))
	require.NoError(t, foreign.Put(ctx, task))
	a, err := s.Claim(ctx, task.NextAttemptAt)
	require.NoError(t, err)
	b, err := foreign.Claim(ctx, task.NextAttemptAt)
	require.NoError(t, err)
	require.NotEmpty(t, a.Receipt.Token)
	require.NotEqual(t, a.Receipt, b.Receipt)
	result := webhook.TerminalResult{State: webhook.DeliverySucceeded}
	for _, receipt := range []webhook.Receipt{{}, {Token: "external-token"}, b.Receipt} {
		require.ErrorIs(t, s.Complete(ctx, receipt, result), webhook.ErrStaleReceipt)
		require.ErrorIs(t, s.Retry(ctx, receipt, time.Now(), webhook.FailureSummary{}), webhook.ErrStaleReceipt)
	}
	require.ErrorIs(t, s.Put(ctx, externalTask(t)), webhook.ErrDeliveryCapacity)
	next := time.Now().Add(time.Hour)
	require.NoError(t, s.Retry(ctx, a.Receipt, next, webhook.FailureSummary{}))
	early, err := s.Claim(ctx, next.Add(-time.Nanosecond))
	require.NoError(t, err)
	require.Nil(t, early)
	a2, err := s.Claim(ctx, next)
	require.NoError(t, err)
	require.Equal(t, 2, a2.Attempts)
	require.NotEqual(t, a.Receipt, a2.Receipt)
	require.ErrorIs(t, s.Complete(ctx, a.Receipt, result), webhook.ErrStaleReceipt)
	outcomes := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); outcomes <- s.Complete(ctx, a2.Receipt, result) }()
	}
	wg.Wait()
	first, second := <-outcomes, <-outcomes
	if first != nil {
		first, second = second, first
	}
	require.NoError(t, first)
	require.ErrorIs(t, second, webhook.ErrStaleReceipt)
	require.NoError(t, s.Put(ctx, task))
	current, err := s.Claim(ctx, time.Now())
	require.NoError(t, err)
	require.NotEqual(t, a2.Receipt, current.Receipt)
	require.ErrorIs(t, s.Complete(ctx, a2.Receipt, result), webhook.ErrStaleReceipt)
	// Final Retry releases the same capacity slot rather than inserting a task.
	require.NoError(t, s.Retry(ctx, current.Receipt, time.Now(), webhook.FailureSummary{}))
	last, err := s.Claim(ctx, time.Now())
	require.NoError(t, err)
	require.NoError(t, s.Retry(ctx, last.Receipt, time.Now(), webhook.FailureSummary{}))
	require.NoError(t, s.Drain(ctx))
	require.NoError(t, s.Put(ctx, task))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	discarded, err := s.Close(cancelled)
	require.NoError(t, err, "local cleanup still runs after cancellation")
	require.Equal(t, 1, discarded)
	discarded, err = s.Close(ctx)
	require.NoError(t, err)
	require.Zero(t, discarded)
	require.ErrorIs(t, s.Put(ctx, task), webhook.ErrDeliveryClosed)
	_, err = s.Claim(ctx, time.Now())
	require.ErrorIs(t, err, webhook.ErrDeliveryClosed)
	require.ErrorIs(t, s.Complete(ctx, current.Receipt, result), webhook.ErrDeliveryClosed)
	require.ErrorIs(t, s.Retry(ctx, current.Receipt, time.Now(), webhook.FailureSummary{}), webhook.ErrDeliveryClosed)
	_, err = foreign.Close(ctx)
	require.NoError(t, err)
}

func TestMemoryDrainWaitsWithoutAdvancingBackoff(t *testing.T) {
	ctx := context.Background()
	s, err := webhook.NewMemoryDeliveryStore(1)
	require.NoError(t, err)
	task := externalTask(t)
	task.NextAttemptAt = time.Now().Add(time.Hour)
	require.NoError(t, s.Put(ctx, task))
	deadline, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Drain(deadline), context.DeadlineExceeded)
	early, err := s.Claim(ctx, time.Now())
	require.NoError(t, err)
	require.Nil(t, early)
	done := make(chan error, 1)
	drainCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	go func() { done <- s.Drain(drainCtx) }()
	claimed, err := s.Claim(ctx, task.NextAttemptAt)
	require.NoError(t, err)
	require.NoError(t, s.Complete(ctx, claimed.Receipt, webhook.TerminalResult{State: webhook.DeliverySucceeded}))
	require.NoError(t, <-done)
	_, err = s.Close(ctx)
	require.NoError(t, err)
}

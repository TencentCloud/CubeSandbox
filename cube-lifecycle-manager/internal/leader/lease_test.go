// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package leader

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeLeaseStore struct {
	mu       sync.Mutex
	owner    string
	renewOK  bool
	acquires int
	renews   int
	releases int
}

type delayedLeaseStore struct {
	delay time.Duration
}

func (s delayedLeaseStore) Acquire(_ context.Context, _ string, _ time.Duration) (bool, error) {
	time.Sleep(s.delay)
	return true, nil
}

func (s delayedLeaseStore) Renew(_ context.Context, _ string, _ time.Duration) (bool, error) {
	time.Sleep(s.delay)
	return true, nil
}

func (s delayedLeaseStore) Release(_ context.Context, _ string) error { return nil }

func (s *fakeLeaseStore) Acquire(_ context.Context, token string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquires++
	if s.owner != "" {
		return false, nil
	}
	s.owner = token
	if !s.renewOK {
		s.renewOK = true
	}
	return true, nil
}

func (s *fakeLeaseStore) Renew(_ context.Context, token string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renews++
	return s.owner == token && s.renewOK, nil
}

func (s *fakeLeaseStore) Release(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == token {
		s.owner = ""
		s.releases++
	}
	return nil
}

func (s *fakeLeaseStore) setRenewOK(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewOK = ok
}

func newTestLease(identity string, store leaseStore) *Lease {
	l := New(Options{
		Enabled:       true,
		Identity:      identity,
		TTL:           100 * time.Millisecond,
		RenewInterval: 10 * time.Millisecond,
		RetryInterval: 5 * time.Millisecond,
		Log:           zap.NewNop(),
	})
	l.store = store
	return l
}

func TestDisabledElectionIsAlwaysLeader(t *testing.T) {
	l := New(Options{Enabled: false, Identity: "single", Log: zap.NewNop()})
	assert.True(t, l.IsLeader())
	assert.False(t, l.Enabled())
	assert.Equal(t, "leader", l.Role())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, l.Run(ctx), context.Canceled)
}

func TestOnlyOneLeaseBecomesLeaderAndStandbyTakesOver(t *testing.T) {
	store := &fakeLeaseStore{renewOK: true}
	first := newTestLease("first", store)
	second := newTestLease("second", store)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- first.Run(firstCtx) }()
	go func() { secondDone <- second.Run(secondCtx) }()

	require.Eventually(t, func() bool {
		return first.IsLeader() != second.IsLeader()
	}, time.Second, 5*time.Millisecond)

	if first.IsLeader() {
		cancelFirst()
		require.ErrorIs(t, <-firstDone, context.Canceled)
		require.Eventually(t, second.IsLeader, time.Second, 5*time.Millisecond)
	} else {
		cancelSecond()
		require.ErrorIs(t, <-secondDone, context.Canceled)
		require.Eventually(t, first.IsLeader, time.Second, 5*time.Millisecond)
		cancelFirst()
		require.ErrorIs(t, <-firstDone, context.Canceled)
		return
	}

	cancelSecond()
	require.ErrorIs(t, <-secondDone, context.Canceled)
}

func TestRenewFailureDemotesImmediately(t *testing.T) {
	store := &fakeLeaseStore{renewOK: true}
	l := newTestLease("candidate", store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	require.Eventually(t, l.IsLeader, time.Second, 5*time.Millisecond)
	store.setRenewOK(false)
	require.Eventually(t, func() bool { return !l.IsLeader() }, time.Second, 5*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestExpiredLocalDeadlineDemotesWithoutWaitingForRedis(t *testing.T) {
	l := newTestLease("candidate", &fakeLeaseStore{renewOK: true})
	expired := time.Now().Add(-time.Millisecond)
	l.deadline.Store(&expired)
	l.leader.Store(true)

	assert.False(t, l.IsLeader())
	assert.Equal(t, "standby", l.Role())
}

func TestLateAcquireResponseIsRejected(t *testing.T) {
	l := New(Options{
		Enabled:       true,
		Identity:      "slow",
		TTL:           30 * time.Millisecond,
		RenewInterval: 10 * time.Millisecond,
		RetryInterval: 5 * time.Millisecond,
		Log:           zap.NewNop(),
	})
	l.store = delayedLeaseStore{delay: 25 * time.Millisecond}

	acquired, err := l.acquire(context.Background())
	assert.False(t, acquired)
	require.ErrorIs(t, err, errLeaseExpired)
	assert.False(t, l.IsLeader())
}

func TestTokenSafeReleaseDoesNotDeleteNewOwner(t *testing.T) {
	store := &fakeLeaseStore{owner: "new-owner", renewOK: true}
	require.NoError(t, store.Release(context.Background(), "old-owner"))

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, "new-owner", store.owner)
	assert.Zero(t, store.releases)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package leaderelect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeLockStore simulates the Redis lease key: a single holder with an
// expiry, honoring the same compare-and-extend / compare-and-delete
// semantics the Lua scripts enforce in production.
type fakeLockStore struct {
	mu       sync.Mutex
	holder   string
	hasKey   bool
	expireAt time.Time

	renewErr   error // injected transport error
	acquires   int
	renewOK    int
	releases   int
	releasedTo string
}

func (f *fakeLockStore) tryAcquire(_ context.Context, _, id string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasKey && time.Now().Before(f.expireAt) {
		return false, nil
	}
	f.hasKey, f.holder = true, id
	f.expireAt = time.Now().Add(ttl)
	f.acquires++
	return true, nil
}

func (f *fakeLockStore) renew(_ context.Context, _, id string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewErr != nil {
		return false, f.renewErr
	}
	if !f.hasKey || time.Now().After(f.expireAt) || f.holder != id {
		return false, nil
	}
	f.expireAt = time.Now().Add(ttl)
	f.renewOK++
	return true, nil
}

func (f *fakeLockStore) release(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	if f.hasKey && f.holder == id {
		f.hasKey, f.holder = false, ""
		f.releasedTo = id
	}
	return nil
}

// steal simulates a peer acquiring the lease after this holder's expiry.
func (f *fakeLockStore) steal(by string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasKey, f.holder = true, by
	f.expireAt = time.Now().Add(time.Hour)
}

func (f *fakeLockStore) counts() (acquires, renewOK, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquires, f.renewOK, f.releases
}

func (f *fakeLockStore) setRenewErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewErr = err
}

func testConfig() Config {
	return Config{
		Key:           "test:leader",
		InstanceID:    "inst-a",
		TTL:           300 * time.Millisecond,
		RenewInterval: 30 * time.Millisecond,
	}
}

func pollTrue(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestConfigValidate(t *testing.T) {
	cfg := testConfig()
	require.NoError(t, cfg.validate())

	bad := cfg
	bad.Key = ""
	assert.Error(t, bad.validate())
	bad = cfg
	bad.InstanceID = ""
	assert.Error(t, bad.validate())
	bad = cfg
	bad.RenewInterval = bad.TTL
	assert.Error(t, bad.validate())
	bad = cfg
	bad.TTL = 0
	assert.Error(t, bad.validate())
}

func TestElectorAcquiresAndRenews(t *testing.T) {
	store := &fakeLockStore{}
	el, err := NewWithStore(store, testConfig(), zap.NewNop())
	require.NoError(t, err)

	elected := make(chan context.Context, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- el.Run(ctx, Callbacks{OnElected: func(c context.Context) { elected <- c }}) }()

	require.True(t, pollTrue(2*time.Second, el.IsLeader), "should acquire leadership")
	var leaderCtx context.Context
	select {
	case leaderCtx = <-elected:
	case <-time.After(time.Second):
		t.Fatal("OnElected not invoked")
	}

	// Stay alive well past one TTL: renewals must keep the lease (and the
	// leader context) alive.
	time.Sleep(2 * testConfig().TTL)
	assert.True(t, el.IsLeader())
	assert.NoError(t, leaderCtx.Err())
	_, renewOK, _ := store.counts()
	assert.Greater(t, renewOK, 1, "lease should have been renewed repeatedly")

	cancel()
	<-done
	assert.False(t, el.IsLeader())
}

func TestElectorLosesLeadershipWhenLeaseStolen(t *testing.T) {
	store := &fakeLockStore{}
	el, err := NewWithStore(store, testConfig(), zap.NewNop())
	require.NoError(t, err)

	elected := make(chan context.Context, 1)
	lost := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- el.Run(ctx, Callbacks{
			OnElected: func(c context.Context) { elected <- c },
			OnLost:    func() { lost <- struct{}{} },
		})
	}()

	require.True(t, pollTrue(2*time.Second, el.IsLeader))
	var leaderCtx context.Context
	select {
	case leaderCtx = <-elected:
	case <-time.After(time.Second):
		t.Fatal("OnElected not invoked")
	}

	store.steal("inst-b")
	require.True(t, pollTrue(2*time.Second, func() bool { return !el.IsLeader() }),
		"should detect the stolen lease on the next renew")
	select {
	case <-lost:
	default:
		t.Fatal("OnLost not invoked")
	}
	assert.Error(t, leaderCtx.Err(), "leader context must be cancelled on loss")

	// The lease now belongs to inst-b for an hour; we must not grab it back.
	assert.False(t, el.IsLeader())
	cancel()
	<-done
}

func TestElectorToleratesTransientRenewErrors(t *testing.T) {
	store := &fakeLockStore{}
	cfg := testConfig()
	el, err := NewWithStore(store, cfg, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- el.Run(ctx, Callbacks{}) }()

	require.True(t, pollTrue(2*time.Second, el.IsLeader))

	// A blip shorter than the TTL must not cost leadership.
	store.setRenewErr(errors.New("redis down"))
	time.Sleep(cfg.TTL / 2)
	assert.True(t, el.IsLeader(), "transient renew failures within TTL must be tolerated")
	store.setRenewErr(nil)
	require.True(t, pollTrue(2*time.Second, func() bool {
		_, renewOK, _ := store.counts()
		return renewOK > 0
	}))

	// Failures lasting past the TTL must cost leadership: by then the lease
	// has expired and a peer may legitimately hold it.
	store.setRenewErr(errors.New("redis down"))
	require.True(t, pollTrue(3*time.Second, func() bool { return !el.IsLeader() }),
		"renewals failing past TTL should drop leadership")
	cancel()
	<-done
}

func TestElectorStepDownReleasesThenReacquires(t *testing.T) {
	store := &fakeLockStore{}
	el, err := NewWithStore(store, testConfig(), zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- el.Run(ctx, Callbacks{}) }()

	require.True(t, pollTrue(2*time.Second, el.IsLeader))
	el.StepDown()
	require.True(t, pollTrue(2*time.Second, func() bool {
		_, _, releases := store.counts()
		return releases >= 1
	}), "step-down should release the lease promptly")

	// With no peer competing, the same instance wins the next acquisition.
	require.True(t, pollTrue(2*time.Second, func() bool {
		acquires, _, _ := store.counts()
		return acquires >= 2
	}), "should re-acquire after a voluntary step-down")
	require.True(t, pollTrue(2*time.Second, el.IsLeader))
	cancel()
	<-done
}

func TestElectorReleasesOnShutdown(t *testing.T) {
	store := &fakeLockStore{}
	el, err := NewWithStore(store, testConfig(), zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- el.Run(ctx, Callbacks{}) }()

	require.True(t, pollTrue(2*time.Second, el.IsLeader))
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	_, _, releases := store.counts()
	assert.Equal(t, 1, releases, "shutdown must release the lease so standbys promote fast")
	assert.False(t, store.hasKey)
}

func TestElectorFencingTokenUniquePerProcess(t *testing.T) {
	cfg := testConfig()
	elA, err := NewWithStore(&fakeLockStore{}, cfg, zap.NewNop())
	require.NoError(t, err)
	elB, err := NewWithStore(&fakeLockStore{}, cfg, zap.NewNop())
	require.NoError(t, err)

	// Same InstanceID, different lock values: the token is the instance ID
	// plus a per-process random suffix.
	assert.True(t, strings.HasPrefix(elA.token, cfg.InstanceID+":"))
	assert.True(t, strings.HasPrefix(elB.token, cfg.InstanceID+":"))
	assert.NotEqual(t, elA.token, elB.token)

	// A same-named peer must never renew or release a lease it doesn't hold.
	store := &fakeLockStore{}
	ctx := context.Background()
	ok, err := store.tryAcquire(ctx, cfg.Key, elA.token, cfg.TTL)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.renew(ctx, cfg.Key, elB.token, cfg.TTL)
	require.NoError(t, err)
	assert.False(t, ok, "peer with same instance ID must not renew our lease")
	require.NoError(t, store.release(ctx, cfg.Key, elB.token))
	assert.True(t, store.hasKey, "peer with same instance ID must not release our lease")
}

func TestElectorDrainsStintBeforeOnLost(t *testing.T) {
	store := &fakeLockStore{}
	el, err := NewWithStore(store, testConfig(), zap.NewNop())
	require.NoError(t, err)

	stintReturned := make(chan struct{})
	onLostSawDrain := make(chan bool, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- el.Run(ctx, Callbacks{
			OnElected: func(c context.Context) {
				<-c.Done()
				// Simulate a leader loop finishing in-flight work after
				// cancellation.
				time.Sleep(100 * time.Millisecond)
				close(stintReturned)
			},
			OnLost: func() {
				select {
				case <-stintReturned:
					onLostSawDrain <- true
				default:
					onLostSawDrain <- false
				}
			},
		})
	}()

	require.True(t, pollTrue(2*time.Second, el.IsLeader))
	store.steal("inst-b")
	select {
	case saw := <-onLostSawDrain:
		assert.True(t, saw, "OnLost must run after the demoted stint has drained")
	case <-time.After(2 * time.Second):
		t.Fatal("OnLost not invoked")
	}
	cancel()
	<-done
}

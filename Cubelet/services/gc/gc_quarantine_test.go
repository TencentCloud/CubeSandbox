// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func newTestGC(t *testing.T) *local {
	t.Helper()
	db, err := utils.NewCubeStoreExt(t.TempDir(), "meta.db", 2, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &local{db: db}
}

type testGauge struct {
	mu    sync.Mutex
	value float64
}

func (g *testGauge) Inc(values ...float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(values) == 0 {
		g.value++
		return
	}
	for _, value := range values {
		g.value += value
	}
}

func (g *testGauge) Dec(values ...float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(values) == 0 {
		g.value--
		return
	}
	for _, value := range values {
		g.value -= value
	}
}

// Add has no caller in this test file today, but is required to satisfy
// metrics.Gauge, which testGauge stands in for.
func (g *testGauge) Add(value float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value += value
}

func (g *testGauge) Set(value float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value = value
}

func (g *testGauge) Value() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.value
}

func installTestGauge(t *testing.T, initial float64) *testGauge {
	t.Helper()
	previousGauge := quarantinedSandbox
	gauge := &testGauge{value: initial}
	quarantinedSandbox = gauge
	t.Cleanup(func() { quarantinedSandbox = previousGauge })
	return gauge
}

func TestRecordCleanupFailureQuarantinesAtTheBound(t *testing.T) {
	l := newTestGC(t)
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-stuck", Namespace: "default"}))

	const maxAttempts = 3
	for i := 1; i < maxAttempts; i++ {
		info, quarantined, err := l.recordCleanupFailure("sb-stuck", maxAttempts)
		require.NoError(t, err)
		assert.False(t, quarantined, "attempt %d is still within the budget", i)
		assert.Equal(t, i, info.Attempts)
		assert.False(t, info.FirstFailedAt.IsZero(), "the stuck-since timestamp must be set on the first failure")
	}

	info, quarantined, err := l.recordCleanupFailure("sb-stuck", maxAttempts)
	require.NoError(t, err)
	assert.True(t, quarantined, "the bound must be reached exactly once")
	require.NotNil(t, info.QuarantinedAt)

	// Entering quarantine is a one-time transition: later failures must not
	// re-trigger the alert.
	_, quarantined, err = l.recordCleanupFailure("sb-stuck", maxAttempts)
	require.NoError(t, err)
	assert.False(t, quarantined)

	persisted, err := l.readSandBoxInfo("sb-stuck")
	require.NoError(t, err)
	assert.True(t, persisted.quarantined(), "quarantine must survive a reload, or a restart would resume retrying")
	assert.Equal(t, maxAttempts, persisted.Attempts, "attempts must stop accruing once quarantined")
}

// Quarantine slows retries down, it does not stop them: an operator who kills
// the holder must not also have to un-quarantine the sandbox by hand.
func TestQuarantinedSandboxIsStillRetriedSlowly(t *testing.T) {
	const interval = 10 * time.Minute
	now := time.Now()

	notQuarantined := &sandBoxInfo{SandboxID: "sb-normal", LastFailedAt: now}
	assert.True(t, notQuarantined.dueForRetryAt(now, interval), "a healthy retry must not be paced")

	justFailed := &sandBoxInfo{SandboxID: "sb-q", QuarantinedAt: &now, LastFailedAt: now}
	assert.False(t, justFailed.dueForRetryAt(now.Add(interval-time.Nanosecond), interval))
	assert.True(t, justFailed.dueForRetryAt(now.Add(interval), interval))

	stale := &sandBoxInfo{SandboxID: "sb-q", QuarantinedAt: &now, LastFailedAt: now.Add(-interval - time.Second)}
	assert.True(t, stale.dueForRetryAt(now, interval), "the slow retry is what lets a reclaimed sandbox recover on its own")
}

func TestLegacyQuarantinedSandboxUsesQuarantinedAtForPacing(t *testing.T) {
	const interval = 10 * time.Minute
	quarantinedAt := time.Date(2026, time.September, 19, 14, 30, 0, 0, time.UTC)
	info := &sandBoxInfo{SandboxID: "sb-legacy", QuarantinedAt: &quarantinedAt}

	assert.False(t, info.dueForRetryAt(quarantinedAt.Add(interval-time.Nanosecond), interval))
	assert.True(t, info.dueForRetryAt(quarantinedAt.Add(interval), interval))
}

// Each failure while quarantined must refresh the pacing timestamp, otherwise
// the slow retry silently reverts to the full 5s rate.
func TestFailureWhileQuarantinedRefreshesPacingWithoutRealerting(t *testing.T) {
	l := newTestGC(t)
	quarantinedAt := time.Now().Add(-time.Hour)
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{
		SandboxID:     "sb-q",
		Attempts:      60,
		FirstFailedAt: quarantinedAt,
		LastFailedAt:  quarantinedAt,
		QuarantinedAt: &quarantinedAt,
	}))

	info, justQuarantined, err := l.recordCleanupFailure("sb-q", 60)
	require.NoError(t, err)
	assert.False(t, justQuarantined, "the alert must fire once, not on every slow retry")
	assert.WithinDuration(t, time.Now(), info.LastFailedAt, time.Second)
	assert.False(t, info.dueForRetryAt(time.Now(), 10*time.Minute), "pacing must restart after the retry")
}

// maxAttempts of 0 is the escape hatch that restores unbounded retrying.
func TestRecordCleanupFailureUnboundedWhenDisabled(t *testing.T) {
	l := newTestGC(t)
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-forever"}))

	for i := 0; i < 100; i++ {
		_, quarantined, err := l.recordCleanupFailure("sb-forever", 0)
		require.NoError(t, err)
		require.False(t, quarantined)
	}
}

// A destroy that keeps failing re-enters the sandbox through Create. If that
// reset the counter, the retry bound could never be reached.
func TestCreatePreservesFailureHistory(t *testing.T) {
	l := newTestGC(t)
	stuckSince := time.Now().Add(-time.Hour)
	lastFailedAt := time.Now().Add(-time.Minute)
	quarantinedAt := time.Now().Add(-30 * time.Minute)
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{
		SandboxID:     "sb-requeued",
		Namespace:     "default",
		Attempts:      7,
		FirstFailedAt: stuckSince,
		LastFailedAt:  lastFailedAt,
		QuarantinedAt: &quarantinedAt,
	}))

	fresh := &sandBoxInfo{SandboxID: "sb-requeued", Namespace: "default"}
	require.NoError(t, l.createSandBoxInfo(fresh))

	info, err := l.readSandBoxInfo("sb-requeued")
	require.NoError(t, err)
	assert.Equal(t, 7, info.Attempts, "the count must continue, not restart")
	assert.WithinDuration(t, stuckSince, info.FirstFailedAt, time.Second)
	assert.WithinDuration(t, lastFailedAt, info.LastFailedAt, time.Second)
	require.NotNil(t, info.QuarantinedAt)
	assert.WithinDuration(t, quarantinedAt, *info.QuarantinedAt, time.Second)
}

func TestCountQuarantined(t *testing.T) {
	l := newTestGC(t)
	now := time.Now()
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-ok"}))
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-q1", QuarantinedAt: &now}))
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-q2", QuarantinedAt: &now}))

	count, err := l.countQuarantined()
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// A single record that fails to decode must not stop every other sandbox
// from being cleaned up, and must not stop countQuarantined (called at
// cubelet startup to seed the gauge) from working either.
func TestReadAllSkipsMalformedRecord(t *testing.T) {
	l := newTestGC(t)
	now := time.Now()
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{SandboxID: "sb-good", QuarantinedAt: &now}))
	require.NoError(t, l.db.Set(bucketName, "sb-corrupt", []byte("not json")))

	infos, err := l.readAll()
	require.NoError(t, err, "one corrupt record must not fail the whole read")
	require.Len(t, infos, 1)
	assert.Equal(t, "sb-good", infos[0].SandboxID)

	count, err := l.countQuarantined()
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestDeleteQuarantinedSandboxDecrementsGaugeExactlyOnce(t *testing.T) {
	l := newTestGC(t)
	now := time.Now()
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{
		SandboxID:     "sb-q",
		QuarantinedAt: &now,
	}))

	gauge := installTestGauge(t, 1)

	require.NoError(t, l.deleteSandBoxInfo("sb-q"))
	assert.Equal(t, float64(0), gauge.Value())

	require.NoError(t, l.deleteSandBoxInfo("sb-q"))
	assert.Equal(t, float64(0), gauge.Value(), "repeated deletion must not decrement the gauge twice")
}

func TestConcurrentDestroyAndCleanupDecrementGaugeOnce(t *testing.T) {
	l := newTestGC(t)
	now := time.Now()
	const sandboxID = "sb-concurrent"
	require.NoError(t, l.saveSandBoxInfo(&sandBoxInfo{
		SandboxID:     sandboxID,
		Namespace:     "default",
		QuarantinedAt: &now,
	}))
	gauge := installTestGauge(t, 1)

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(cleanup bool) {
			defer wg.Done()
			if cleanup {
				errs <- l.CleanUp(context.Background(), &workflow.CleanContext{
					BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
				})
				return
			}
			errs <- l.Destroy(context.Background(), &workflow.DestroyContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
			})
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, float64(0), gauge.Value())
	_, err := l.readSandBoxInfo(sandboxID)
	assert.ErrorIs(t, err, utils.ErrorKeyNotFound)
}

func TestCountQuarantinedAfterDatabaseReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := utils.NewCubeStoreExt(dir, "meta.db", 2, nil)
	require.NoError(t, err)
	first := &local{db: db}
	now := time.Now()
	require.NoError(t, first.saveSandBoxInfo(&sandBoxInfo{
		SandboxID:     "sb-restart",
		QuarantinedAt: &now,
	}))
	require.NoError(t, db.Close())

	reopened, err := utils.NewCubeStoreExt(dir, "meta.db", 2, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	second := &local{db: reopened}
	count, err := second.countQuarantined()
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestApplyGCServiceDefaults(t *testing.T) {
	maxAttempts := 3
	tests := []struct {
		name               string
		config             GCServicesConfig
		cleanupInterval    time.Duration
		quarantineInterval time.Duration
		maxAttempts        int
	}{
		{
			name:               "defaults",
			config:             GCServicesConfig{},
			cleanupInterval:    defaultCleanupInterval,
			quarantineInterval: defaultQuarantineRetryInterval,
			maxAttempts:        defaultMaxCleanupAttempts,
		},
		{
			name: "configured",
			config: GCServicesConfig{
				CleanupIntervalStr:         "2s",
				QuarantineRetryIntervalStr: "15s",
				MaxCleanupAttempts:         &maxAttempts,
			},
			cleanupInterval:    2 * time.Second,
			quarantineInterval: 15 * time.Second,
			maxAttempts:        3,
		},
		{
			name: "invalid and negative durations use defaults",
			config: GCServicesConfig{
				CleanupIntervalStr:         "-1s",
				QuarantineRetryIntervalStr: "invalid",
			},
			cleanupInterval:    defaultCleanupInterval,
			quarantineInterval: defaultQuarantineRetryInterval,
			maxAttempts:        defaultMaxCleanupAttempts,
		},
		{
			name: "quarantine retry is never faster than cleanup",
			config: GCServicesConfig{
				CleanupIntervalStr:         "10s",
				QuarantineRetryIntervalStr: "1s",
			},
			cleanupInterval:    10 * time.Second,
			quarantineInterval: 10 * time.Second,
			maxAttempts:        defaultMaxCleanupAttempts,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := test.config
			applyGCServiceDefaults(&config)
			assert.Equal(t, test.cleanupInterval, config.cleanupInterval)
			assert.Equal(t, test.quarantineInterval, config.quarantineRetryInterval)
			assert.Equal(t, test.maxAttempts, config.maxCleanupAttempts)
		})
	}
}

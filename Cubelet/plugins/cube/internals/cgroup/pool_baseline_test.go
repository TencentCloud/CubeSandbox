// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

// baselineTestHandle is a fake handle.Interface for driving the pool state
// machine without a real cgroup filesystem.
type baselineTestHandle struct {
	handle.Interface

	cleanErr      error
	persistentErr error // if set, every UsageSnapshot call fails
	usageValue    handle.UsageSnapshot
	usageErrs     []error // per-call scripted errors; nil entry means success
	usageCalls    int
	created       bool
}

func (h *baselineTestHandle) IsExist(_ context.Context, _ string) bool {
	return h.created
}

func (h *baselineTestHandle) Create(_ context.Context, _ string) error {
	h.created = true
	return nil
}

func (h *baselineTestHandle) CleanForReuse(_ context.Context, _ string) error {
	return h.cleanErr
}

func (h *baselineTestHandle) UsageSnapshot(_ context.Context, _ string) (handle.UsageSnapshot, error) {
	defer func() { h.usageCalls++ }()
	if h.persistentErr != nil {
		return handle.UsageSnapshot{}, h.persistentErr
	}
	if h.usageCalls < len(h.usageErrs) && h.usageErrs[h.usageCalls] != nil {
		return handle.UsageSnapshot{}, h.usageErrs[h.usageCalls]
	}
	return h.usageValue, nil
}

func (h *baselineTestHandle) List() ([]string, error)               { return nil, nil }
func (h *baselineTestHandle) ListSubdir(_ string) ([]string, error) { return nil, nil }

func newBaselineTestPool(t *testing.T, size int, h handle.Interface) *cgPool {
	t.Helper()
	ranger, err := allocator.NewSimpleLinearRanger(0, uint16(size))
	require.NoError(t, err)
	return &cgPool{
		initialSize:  size,
		poolV1Handle: h,
		poolV2Handle: h,
		poolV1: cgPoolV1{
			allocator: allocator.NewAllocator[uint16](ranger),
			cgRanger:  ranger,
		},
		dirtySet:            make(map[uint32]struct{}),
		pendingSet:          make(map[uint32]struct{}),
		baselineCache:       make(map[uint32]handle.UsageSnapshot),
		individualGroups:    make(map[string]struct{}),
		queue:               newIDQueue(),
		stopCh:              make(chan struct{}),
		workers:             1,
		triggerInterval:     10 * time.Millisecond,
		reparentFileEnabled: func() bool { return false },
	}
}

// tryPop is a non-blocking pop for tests.
func (q *idQueue) tryPop() (uint32, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return 0, false
	}
	id := q.items[0]
	q.items = q.items[1:]
	if len(q.items) == 0 {
		q.items = nil
	}
	delete(q.queued, id)
	return id, true
}

// drainBaselineQueue runs the workers' step synchronously.
func drainBaselineQueue(p *cgPool) {
	for {
		id, ok := p.queue.tryPop()
		if !ok {
			return
		}
		p.finishBaseline(context.Background(), id)
	}
}

func TestPutDefersReleaseUntilBaselineRead(t *testing.T) {
	h := &baselineTestHandle{usageValue: handle.UsageSnapshot{CPUUsageTotalNS: 42}, created: true}
	p := newBaselineTestPool(t, 4, h)

	cg, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)
	require.Equal(t, uint16(0), *cg)

	p.Put(context.Background(), uint32(0))

	// The slot is cleaned and queued, but NOT released yet.
	assert.Contains(t, p.pendingSet, uint32(0))
	assert.ElementsMatch(t, []uint32{0}, p.All())
	assert.Equal(t, 1, p.queue.size())

	drainBaselineQueue(p)

	assert.NotContains(t, p.pendingSet, uint32(0))
	assert.Empty(t, p.All())
	assert.Equal(t, handle.UsageSnapshot{CPUUsageTotalNS: 42}, p.baselineCache[uint32(0)])
}

func TestPutCleanFailureKeepsSlotDirty(t *testing.T) {
	h := &baselineTestHandle{cleanErr: fmt.Errorf("kill failed"), created: true}
	p := newBaselineTestPool(t, 4, h)

	cg, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)
	require.Equal(t, uint16(0), *cg)

	p.Put(context.Background(), uint32(0))

	assert.Contains(t, p.dirtySet, uint32(0))
	assert.NotContains(t, p.pendingSet, uint32(0))
	assert.ElementsMatch(t, []uint32{0}, p.All())
	assert.Equal(t, 0, p.queue.size())
}

func TestBaselineReadFailureReturnsReturnedSlotToPending(t *testing.T) {
	h := &baselineTestHandle{usageErrs: []error{fmt.Errorf("transient read failure")}, created: true}
	p := newBaselineTestPool(t, 4, h)

	cg, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)
	require.Equal(t, uint16(0), *cg)

	p.Put(context.Background(), uint32(0))
	drainBaselineQueue(p)

	// Read failed: slot goes back to pending, stays assigned, no baseline.
	assert.Contains(t, p.pendingSet, uint32(0))
	assert.ElementsMatch(t, []uint32{0}, p.All())
	assert.NotContains(t, p.baselineCache, uint32(0))

	// The maintenance tick re-enqueues it, and the next read succeeds.
	p.runMaintenance()
	assert.Equal(t, 1, p.queue.size())
	drainBaselineQueue(p)
	assert.NotContains(t, p.pendingSet, uint32(0))
	assert.Empty(t, p.All())
}

func TestBaselineReadENOENTRecreatesCgroup(t *testing.T) {
	h := &baselineTestHandle{
		usageErrs:  []error{fmt.Errorf("read cpu.max: no such file or directory")},
		usageValue: handle.UsageSnapshot{CPUUsageTotalNS: 7},
		created:    false, // the cgroup is gone
	}
	p := newBaselineTestPool(t, 4, h)

	cg, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)
	require.Equal(t, uint16(0), *cg)

	p.Put(context.Background(), uint32(0))
	drainBaselineQueue(p)

	assert.True(t, h.created, "missing cgroup must be recreated")
	assert.Equal(t, handle.UsageSnapshot{CPUUsageTotalNS: 7}, p.baselineCache[uint32(0)])
	assert.Empty(t, p.All(), "recreated cgroup releases its slot with a fresh baseline")
}

func TestBaselineForCacheHitThenFallback(t *testing.T) {
	h := &baselineTestHandle{
		usageErrs:  []error{fmt.Errorf("first read fails")},
		usageValue: handle.UsageSnapshot{CPUUsageTotalNS: 99},
		created:    true,
	}
	p := newBaselineTestPool(t, 4, h)

	// Miss: falls back to a synchronous read with retries.
	usage, cached, err := p.BaselineFor(context.Background(), uint32(0))
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, uint64(99), usage.CPUUsageTotalNS)
	assert.Equal(t, 2, h.usageCalls, "first attempt fails, retry succeeds")

	// Hit: cached counters pop without touching the handle.
	p.baselineCache[uint32(1)] = handle.UsageSnapshot{CPUUsageTotalNS: 11}
	usage, cached, err = p.BaselineFor(context.Background(), uint32(1))
	require.NoError(t, err)
	assert.True(t, cached)
	assert.Equal(t, uint64(11), usage.CPUUsageTotalNS)
	assert.NotContains(t, p.baselineCache, uint32(1), "assignment must pop the entry")
}

func TestBaselineForFallbackExhaustsRetries(t *testing.T) {
	h := &baselineTestHandle{usageErrs: []error{
		fmt.Errorf("fail1"), fmt.Errorf("fail2"), fmt.Errorf("fail3"),
	}, created: true}
	p := newBaselineTestPool(t, 4, h)

	_, _, err := p.BaselineFor(context.Background(), uint32(0))
	require.Error(t, err)
	assert.Equal(t, 3, h.usageCalls)
}

func TestSweepBaselinesSkipsInUseSlots(t *testing.T) {
	h := &baselineTestHandle{usageValue: handle.UsageSnapshot{MemoryCurrentBytes: 5}, created: true}
	p := newBaselineTestPool(t, 4, h)

	require.NoError(t, p.poolV1.initialAssign(2))

	p.sweepBaselines(map[uint32]struct{}{2: {}})
	drainBaselineQueue(p)

	assert.NotContains(t, p.baselineCache, uint32(2), "in-use slot must not get a baseline")
	for _, free := range []uint32{0, 1, 3} {
		assert.Contains(t, p.baselineCache, free)
	}
	assert.ElementsMatch(t, []uint32{2}, p.All(), "sweep must not release in-use slots")
}

func TestQueueDedupsAndCloses(t *testing.T) {
	q := newIDQueue()
	q.push(1)
	q.push(1)
	q.push(2)
	assert.Equal(t, 2, q.size(), "duplicate pushes must be deduped")

	id, ok := q.pop()
	assert.True(t, ok)
	assert.Equal(t, uint32(1), id)
	assert.Equal(t, 1, q.size())

	q.push(1) // popped ids can be re-queued later
	assert.Equal(t, 2, q.size())

	// Closing drops residual items.
	q.close()
	assert.Equal(t, 2, q.size(), "close does not remove buffered items")
	_, ok = q.pop()
	assert.False(t, ok, "a closed queue must not hand out more work")

	q.push(3) // closed queue silently drops
	assert.Equal(t, 2, q.size())
}

func TestMaintenanceRequeueStaysBounded(t *testing.T) {
	h := &baselineTestHandle{persistentErr: fmt.Errorf("persistent failure"), created: true}
	p := newBaselineTestPool(t, 4, h)

	cg, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)
	require.Equal(t, uint16(0), *cg)

	p.Put(context.Background(), uint32(0))
	drainBaselineQueue(p) // fails, back to pending

	for i := 0; i < 5; i++ {
		p.runMaintenance()
	}
	assert.Equal(t, 1, p.queue.size(), "tick re-enqueue must not grow the queue")

	drainBaselineQueue(p)
	assert.Contains(t, p.pendingSet, uint32(0), "failed slot stays pending and unreleased")
	assert.ElementsMatch(t, []uint32{0}, p.All())
}

func TestStartAndStopWorkerLifecycle(t *testing.T) {
	h := &baselineTestHandle{usageValue: handle.UsageSnapshot{CPUUsageTotalNS: 3}, created: true}
	p := newBaselineTestPool(t, 4, h)

	_, err := p.poolV1.Get(context.Background(), "sb1")
	require.NoError(t, err)

	p.start()
	defer p.stop()

	p.Put(context.Background(), uint32(0))

	require.Eventually(t, func() bool {
		p.mutex.RLock()
		defer p.mutex.RUnlock()
		_, released := p.baselineCache[uint32(0)]
		return released
	}, 5*time.Second, 5*time.Millisecond, "worker must cache the baseline and release the slot")

	p.stop()
	_, ok := p.queue.pop()
	assert.False(t, ok, "stopped queue must not hand out more work")
}

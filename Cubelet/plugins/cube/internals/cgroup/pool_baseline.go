// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

// idQueue is an unbounded FIFO of cgroup ids; pushes are deduped so retries
// cannot grow it without bound.
type idQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []uint32
	queued map[uint32]struct{}
	closed bool
}

func newIDQueue() *idQueue {
	q := &idQueue{queued: make(map[uint32]struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *idQueue) push(id uint32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if _, ok := q.queued[id]; ok {
		return
	}
	q.queued[id] = struct{}{}
	q.items = append(q.items, id)
	q.cond.Signal()
}

// pop blocks until an item is available or the queue is closed; a closed
// queue drops residual items.
func (q *idQueue) pop() (uint32, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return 0, false
	}
	id := q.items[0]
	q.items = q.items[1:]
	if len(q.items) == 0 {
		q.items = nil // release the backing array after draining
	}
	delete(q.queued, id)
	return id, true
}

func (q *idQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.cond.Broadcast()
	}
}

func (q *idQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// start launches the baseline workers and the maintenance loop; called once
// at pool init.
func (p *cgPool) start() {
	for i := 0; i < p.workers; i++ {
		go p.workerLoop()
	}
	go p.maintenanceLoop()
}

// stop is idempotent; workers drop work they have not started.
func (p *cgPool) stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.queue.close()
	})
}

func (p *cgPool) workerLoop() {
	for {
		id, ok := p.queue.pop()
		if !ok {
			return
		}
		select {
		case <-p.stopCh:
			return
		default:
		}
		p.runSafely(func() { p.finishBaseline(context.Background(), id) })
	}
}

func (p *cgPool) maintenanceLoop() {
	ticker := time.NewTicker(p.triggerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.runSafely(p.runMaintenance)
		}
	}
}

// runMaintenance re-enqueues pending baseline reads and retries the dirty
// set via Tidy.
func (p *cgPool) runMaintenance() {
	p.mutex.RLock()
	pending := make([]uint32, 0, len(p.pendingSet))
	for id := range p.pendingSet {
		pending = append(pending, id)
	}
	pendingCount := len(p.pendingSet)
	p.mutex.RUnlock()

	for _, id := range pending {
		p.queue.push(id)
	}
	if pendingCount > 0 {
		log.G(context.Background()).Warnf("cgroup pool: %d slot(s) pending baseline retry, %d queued", pendingCount, p.queue.size())
	}

	if errs := p.Tidy(); len(errs) > 0 {
		log.G(context.Background()).Warnf("cgroup pool tidy: %v", errs)
	}
}

// finishBaseline reads an idle slot's frozen counters and releases it with a
// ready baseline. A failed read keeps a returned slot in pendingSet for
// retry.
func (p *cgPool) finishBaseline(ctx context.Context, fullCgID uint32) {
	p.mutex.Lock()
	_, fromReturn := p.pendingSet[fullCgID]
	delete(p.pendingSet, fullCgID)
	p.mutex.Unlock()

	usage, err := p.readUsageSnapshot(ctx, fullCgID)
	if err != nil && isNotExistError(err) {
		// The cgroup vanished; a fresh one reads as all-zero counters,
		// which is a valid baseline.
		if createErr := p.createCgroup(ctx, fullCgID); createErr == nil {
			usage, err = p.readUsageSnapshot(ctx, fullCgID)
		}
	}
	if err != nil {
		if fromReturn {
			p.mutex.Lock()
			p.pendingSet[fullCgID] = struct{}{}
			p.mutex.Unlock()
		}
		// Warn: the tick retries and logs the pending count.
		log.G(ctx).Warnf("read baseline for cgroup %v: %v", MakeCgroupPathByID(fullCgID), err)
		return
	}

	p.mutex.Lock()
	p.baselineCache[fullCgID] = usage
	p.mutex.Unlock()
	if fromReturn {
		p.allocatorReleaseCgroup(fullCgID)
	}
}

func (p *cgPool) readUsageSnapshot(ctx context.Context, fullCgID uint32) (handle.UsageSnapshot, error) {
	group := MakeCgroupPathByID(fullCgID)
	if IsPoolV2ID(fullCgID) {
		return p.poolV2Handle.UsageSnapshot(ctx, group)
	}
	return p.poolV1Handle.UsageSnapshot(ctx, group)
}

// BaselineFor returns the counters an idle slot had when it became idle; on
// a cache miss it falls back to a synchronous read with retries.
func (p *cgPool) BaselineFor(ctx context.Context, fullCgID uint32) (handle.UsageSnapshot, bool, error) {
	p.mutex.Lock()
	usage, ok := p.baselineCache[fullCgID]
	if ok {
		delete(p.baselineCache, fullCgID)
	}
	p.mutex.Unlock()
	if ok {
		return usage, true, nil
	}

	var err error
	for attempt := 1; attempt <= hostMetricsBaselineAttempts; attempt++ {
		usage, err = p.readUsageSnapshot(ctx, fullCgID)
		if err == nil {
			return usage, false, nil
		}
		if attempt == hostMetricsBaselineAttempts {
			break
		}
		timer := time.NewTimer(hostMetricsBaselineRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return handle.UsageSnapshot{}, false, ctx.Err()
		case <-timer.C:
		}
	}
	return handle.UsageSnapshot{}, false, err
}

// sweepBaselines enqueues every idle slot for a baseline read; in-use slots
// are skipped.
func (p *cgPool) sweepBaselines(inUse map[uint32]struct{}) {
	p.mutex.RLock()
	v1Cap := p.poolV1.cgRanger.Cap()
	numaPools := p.poolV2.numaPools
	p.mutex.RUnlock()

	for id := 0; id < v1Cap; id++ {
		full := uint32(id)
		if _, used := inUse[full]; used {
			continue
		}
		p.queue.push(full)
	}
	for _, numaPool := range numaPools {
		nodeID := int32(numaPool.numaNode.NodeId)
		for id := 0; id < numaPool.cgRanger.Cap(); id++ {
			full := MakeFullCgID(uint16(id), true, nodeID)
			if _, used := inUse[full]; used {
				continue
			}
			p.queue.push(full)
		}
	}
}

// runSafely runs a background step; a panic is process-fatal (utils.Recover),
// relying on restart to rebuild pool state.
func (p *cgPool) runSafely(step func()) {
	defer utils.Recover()
	step()
}

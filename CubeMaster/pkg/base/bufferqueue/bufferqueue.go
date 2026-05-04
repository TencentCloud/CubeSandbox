// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package bufferqueue implements a buffer queue.
package bufferqueue

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/semaphore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type WorkHandler interface {
	Handle()
}

type BufferQueue interface {
	PushItem(i *Item)

	Push(value interface{})

	Pop() interface{}

	Len() int

	Workings() int32

	SetLimit(n int64)

	GraceFullStop(ctx context.Context)
}

type Options struct {
	Limit int64

	// testHook is invoked at the end of each consumer loop iteration that
	// dispatched work. Tests in this package set it to observe loop progress;
	// production callers leave it nil.
	testHook func()
}

var errQueueLimitExceeded = errors.New("buffer queue limit exceeded")

func New(opt *Options) BufferQueue {
	if opt == nil {
		panic("opt is nill")
	}

	if opt.Limit <= 0 {
		opt.Limit = 10
	}
	sh := &bufferQueue{
		queue:    &TimeSortedQueue{},
		limiter:  semaphore.NewWeighted(opt.Limit),
		stopped:  make(chan struct{}),
		notify:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		testHook: opt.testHook,
	}
	heap.Init(sh.queue)
	sh.start()
	return sh
}

func (sh *bufferQueue) start() {
	loopFunc := func() {
		for {
			// Wait for work or stop signal when queue is empty.
			if sh.Len() <= 0 {
				if sh.isStopped() {
					return
				}

				select {
				case <-sh.stopped:
				case <-sh.notify:
				}
				continue
			}

			// Fast-path: try non-blocking acquire first to avoid blocking on the limiter.
			if err := sh.TryAcquire(); err != nil {
				if err := sh.Acquire(); err != nil {
					continue
				}
			}

			value := sh.Pop()
			if value == nil {
				sh.Release()
				continue
			}

			wh, ok := value.(WorkHandler)
			if !ok {
				sh.Release()
				CubeLog.WithContext(context.Background()).Warnf(
					"BufferWorker: item does not implement WorkHandler: %v",
					utils.InterfaceToString(value))
				continue
			}

			recov.GoWithWaitGroup(&sh.wg, func() {
				defer sh.Release()
				sh.IncrWorking()
				defer sh.DecrWorking()
				wh.Handle()
			}, func(panicError interface{}) {
				CubeLog.WithContext(context.Background()).Fatalf("BufferWorker panic:%v,value:%v",
					panicError, utils.InterfaceToString(value))
			})

			if sh.testHook != nil {
				sh.testHook()
			}
		}
	}
	recov.GoWithWaitGroup(&sh.wg, loopFunc, func(panicError interface{}) {
		CubeLog.WithContext(context.Background()).Fatalf("BufferWorker panic:%v", panicError)
	})
}

// GraceFullStop signals the consumer loop and any in-flight workers to drain,
// then waits for completion or until ctx is done. Safe to call multiple times.
//
// Note: if ctx fires before the queue drains (e.g. a hung Handle), the internal
// wg.Wait waiter goroutine remains until all workers eventually return — which
// matches the existing trade-off, since WorkHandler has no cancellation channel.
func (sh *bufferQueue) GraceFullStop(ctx context.Context) {
	sh.stopOnce.Do(func() {
		atomic.StoreInt32(&sh.stoppedFlg, 1)
		close(sh.stopped)
		go func() {
			sh.wg.Wait()
			close(sh.done)
		}()
	})

	select {
	case <-ctx.Done():
	case <-sh.done:
	}
}

type bufferQueue struct {
	queue      *TimeSortedQueue
	limiter    *semaphore.Weighted
	lock       sync.RWMutex
	len        int32
	working    int32
	stopped    chan struct{}
	notify     chan struct{}
	done       chan struct{}
	stoppedFlg int32 // atomic flag for fast stopped check without channel read
	stopOnce   sync.Once
	testHook   func() // test only: called at the end of each loop iteration that dispatched work
	wg         sync.WaitGroup
}

func (sh *bufferQueue) isStopped() bool {
	return atomic.LoadInt32(&sh.stoppedFlg) == 1
}

func (sh *bufferQueue) signal() {
	select {
	case sh.notify <- struct{}{}:
	default:
	}
}

func (sh *bufferQueue) PushItem(i *Item) {
	sh.lock.Lock()
	heap.Push(sh.queue, i)
	atomic.AddInt32(&sh.len, 1)
	sh.lock.Unlock()
	sh.signal()
}

func (sh *bufferQueue) Push(value interface{}) {
	sh.lock.Lock()
	item := &Item{
		value:    value,
		priority: time.Now().UnixNano(),
	}
	heap.Push(sh.queue, item)
	atomic.AddInt32(&sh.len, 1)
	sh.lock.Unlock()
	sh.signal()
}

func (sh *bufferQueue) Pop() interface{} {
	sh.lock.Lock()
	defer sh.lock.Unlock()
	if sh.queue.Len() == 0 {
		// Defensive: keep counter aligned with the actual heap state.
		atomic.StoreInt32(&sh.len, 0)
		return nil
	}
	atomic.AddInt32(&sh.len, -1)
	item := heap.Pop(sh.queue)
	if item == nil {
		return nil
	}
	v, ok := item.(*Item)
	if !ok {
		return nil
	}
	return v.value
}

func (sh *bufferQueue) Len() int {
	return int(atomic.LoadInt32(&sh.len))
}

// Acquire blocks until a limiter slot is available. The notify-driven loop no
// longer needs polling, so we use Background to avoid per-call ctx allocation.
func (sh *bufferQueue) Acquire() error {
	if sh.limiter == nil {
		return nil
	}
	return sh.limiter.Acquire(context.Background(), 1)
}

func (sh *bufferQueue) TryAcquire() error {
	if sh.limiter == nil {
		return nil
	}
	if sh.limiter.TryAcquire(1) {
		return nil
	}
	return errQueueLimitExceeded
}

func (sh *bufferQueue) Release() {
	if sh.limiter == nil {
		return
	}
	sh.limiter.Release(1)
}

func (sh *bufferQueue) SetLimit(n int64) {
	if sh.limiter == nil {
		return
	}
	sh.limiter.SetLimit(n)
}

func (sh *bufferQueue) IncrWorking() {
	atomic.AddInt32(&sh.working, 1)
}

func (sh *bufferQueue) DecrWorking() {
	atomic.AddInt32(&sh.working, -1)
}

func (sh *bufferQueue) Workings() int32 {
	return atomic.LoadInt32(&sh.working)
}

type Item struct {
	value    interface{}
	priority int64
	index    int
}

type TimeSortedQueue []*Item

func (pq TimeSortedQueue) Len() int { return len(pq) }

func (pq TimeSortedQueue) Less(i, j int) bool {

	return pq[i].priority < pq[j].priority
}

func (pq TimeSortedQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *TimeSortedQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*Item)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *TimeSortedQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid memory leak
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

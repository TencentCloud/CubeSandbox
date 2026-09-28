// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package bufferqueue implements a buffer queue.
package bufferqueue

import (
	"container/heap"
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/semaphore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type WorkHandler interface {
	Handle()
}

type BufferQueue interface {
	PushItem(i *Item)

	Push(any interface{})

	Pop() interface{}

	Len() int

	Workings() int32

	SetLimit(n int64)

	GraceFullStop(ctx context.Context)
}

type Options struct {
	Limit int64
}

func New(opt *Options) BufferQueue {
	if opt == nil {
		panic("opt is nill")
	}

	if opt.Limit <= 0 {
		opt.Limit = 10
	}
	sh := &bufferQueue{
		queue:   &TimeSortedQueue{},
		limiter: semaphore.NewWeighted(opt.Limit),
		stopped: make(chan struct{}),
	}
	heap.Init(sh.queue)
	sh.start()
	return sh
}

func (sh *bufferQueue) start() {
	randInterval := func() time.Duration {
		return time.Duration(float64(5+rand.Intn(5))*(1+0.8*rand.Float64())) * time.Millisecond
	}
	loopFunc := func() {
		for {
			select {
			case <-sh.stopped:
				if sh.Len() > 0 || sh.Workings() > 0 {
					break
				}
				return
			default:
			}

			if sh.Len() <= 0 {
				time.Sleep(time.Millisecond)
				continue
			}

			if err := sh.TryAcquire(); err != nil {
				time.Sleep(time.Millisecond)
				continue
			}

			value := sh.Pop()
			if value == nil {
				sh.Release()
				time.Sleep(time.Millisecond)
				continue
			}

			wh, ok := value.(WorkHandler)
			if !ok {
				sh.Release()
				time.Sleep(randInterval())
				continue
			}

			recov.GoWithRecover(func() {
				defer sh.Release()
				sh.IncrWorking()
				defer sh.DecrWorking()
				wh.Handle()
			}, func(panicError interface{}) {
				defer sh.Release()
				defer sh.DecrWorking()
				CubeLog.WithContext(context.Background()).Fatalf("BufferWorker panic:%v,value:%v",
					panicError, utils.InterfaceToString(value))
			})

			select {
			case <-sh.stopped:
				if sh.Len() > 0 || sh.Workings() > 0 {

					break
				}
				return
			default:
			}
		}
	}
	recov.GoWithRecover(loopFunc, func(panicError interface{}) {
		CubeLog.WithContext(context.Background()).Fatalf("BufferWorker panic:%v", panicError)
	})
}

func (sh *bufferQueue) GraceFullStop(ctx context.Context) {
	close(sh.stopped)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := sh.Acquire(); err == nil {
			sh.Release()
			if sh.Len() <= 0 && sh.Workings() <= 0 {

				return
			}
		}
		time.Sleep(time.Millisecond * 10)
	}
}

type bufferQueue struct {
	queue   *TimeSortedQueue
	limiter *semaphore.Weighted
	lock    sync.RWMutex
	len     int32
	working int32
	stopped chan struct{}
}

func (sh *bufferQueue) PushItem(i *Item) {
	sh.lock.Lock()
	defer sh.lock.Unlock()
	heap.Push(sh.queue, i)
	atomic.AddInt32(&sh.len, 1)
}

func (sh *bufferQueue) Push(x interface{}) {
	sh.lock.Lock()
	defer sh.lock.Unlock()
	item := &Item{
		value:    x,
		priority: time.Now().UnixNano(),
	}
	heap.Push(sh.queue, item)
	atomic.AddInt32(&sh.len, 1)
}

func (sh *bufferQueue) Pop() interface{} {
	sh.lock.Lock()
	defer sh.lock.Unlock()
	if sh.queue.Len() == 0 {
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

func (sh *bufferQueue) Acquire() error {
	if sh.limiter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return sh.limiter.Acquire(ctx, 1)
}

func (sh *bufferQueue) TryAcquire() error {
	if sh.limiter == nil {
		return nil
	}
	if sh.limiter.TryAcquire(1) {
		return nil
	}
	return errors.New("buffer queue limit exceeded")
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
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package bufferqueue

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type testReq struct {
	id     int
	sleep  time.Duration
	worked *int32
}

func (r *testReq) Handle() {

	time.Sleep(r.sleep)
	if r.worked != nil {
		atomic.AddInt32(r.worked, 1)
	}

}

func TestQueue(t *testing.T) {
	worked := int32(0)
	bw := New(&Options{Limit: 1})
	testnum := 10
	for i := 1; i <= testnum; i++ {
		bw.Push(&testReq{id: i, sleep: time.Second, worked: &worked})
	}
	waittime := time.Duration(testnum+2) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), waittime)
	defer cancel()
	bw.GraceFullStop(ctx)
	assert.Equal(t, int32(testnum), atomic.LoadInt32(&worked))
}

func TestQueueGracefullStopTimeout(t *testing.T) {
	bw := New(&Options{Limit: 5})
	for i := 1; i <= 10; i++ {
		bw.Push(&testReq{id: i, sleep: 3 * time.Second})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bw.GraceFullStop(ctx)
	assert.Truef(t, bw.Workings() > 0, fmt.Sprintf("Workings:%d", bw.Workings()))
	assert.Truef(t, bw.Len() >= 0, fmt.Sprintf("Len:%d", bw.Len()))
}

func TestTimeSortedQueue(t *testing.T) {
	var sortq TimeSortedQueue
	testnum := 3000
	for i := 1; i <= testnum; i++ {
		randt := time.Duration(float64(rand.Intn(5)) * (1 + 0.8*rand.Float64()))
		t := time.Now().Add(-randt)
		item := &Item{
			value:    i,
			priority: t.UnixNano(),
		}
		sortq.Push(item)
		time.Sleep(time.Millisecond * 5)
	}

	for i, v := range sortq {
		assert.LessOrEqual(t, v.priority, sortq[i+1].priority)
		if i == len(sortq)-2 {
			break
		}
	}
}

type dealItems struct {
	sync.Mutex
	list []*testReqWithTimestamp
}

func (d *dealItems) push(item *testReqWithTimestamp) {
	d.Lock()
	defer d.Unlock()
	d.list = append(d.list, item)
}

type testReqWithTimestamp struct {
	id         int
	sleep      time.Duration
	worked     *int32
	createTime time.Time
	dealQ      *dealItems
}

func (r *testReqWithTimestamp) Handle() {
	if r.dealQ != nil {
		r.dealQ.push(r)
	}
	time.Sleep(r.sleep)
	if r.worked != nil {
		atomic.AddInt32(r.worked, 1)
	}
}

func TestQueueCheckTimestampPriority(t *testing.T) {
	worked := int32(0)
	dealQ := &dealItems{}
	testnum := 3000
	bw := New(&Options{Limit: 5})
	for i := 1; i <= testnum; i++ {
		randt := time.Duration(float64(rand.Intn(5)) * (1 + 0.8*rand.Float64()))
		ts := time.Now().Add(-randt)
		item := &Item{
			value:    &testReqWithTimestamp{id: i, sleep: 5 * time.Millisecond, worked: &worked, createTime: ts, dealQ: dealQ},
			priority: ts.UnixNano(),
		}
		bw.PushItem(item)
		time.Sleep(time.Millisecond * 10)
	}

	waittime := time.Duration(testnum*10) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), waittime)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if atomic.LoadInt32(&worked) == int32(testnum) {
					cancel()
					return
				}
				t.Logf("workings:%d", bw.Workings())
			}
		}
	}()

	bw.GraceFullStop(ctx)
	assert.Equal(t, int32(testnum), atomic.LoadInt32(&worked))
	assert.Equal(t, int32(0), bw.Workings())
	assert.Equal(t, int(testnum), len(dealQ.list))
}

// TestQueueIdleCPU verifies that an idle queue blocks on the notify channel
// instead of busy-looping. The consumer goroutine should consume near-zero CPU
// when no items are enqueued.
func TestQueueIdleCPU(t *testing.T) {
	var loopCount int32
	bw := New(&Options{
		Limit: 5,
		testHook: func() {
			atomic.AddInt32(&loopCount, 1)
		},
	})

	// Let the consumer goroutine idle for a reasonable window.
	time.Sleep(200 * time.Millisecond)

	// In an idle state, the loop should be blocked at the select statement
	// before ever completing its first iteration.
	assert.Equal(t, int32(0), atomic.LoadInt32(&loopCount), "Main loop should not spin while idle")
	assert.Equal(t, 0, bw.Len())
	assert.Equal(t, int32(0), bw.Workings())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bw.GraceFullStop(ctx)
}

// TestQueueDelayedPushWakesConsumer verifies that a Push into an idle queue
// correctly signals the consumer goroutine via the notify channel and the item
// gets processed promptly.
func TestQueueDelayedPushWakesConsumer(t *testing.T) {
	worked := int32(0)
	bw := New(&Options{Limit: 5})

	// Let the consumer enter its blocking select on the empty queue.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, bw.Len())

	// Now push an item; the consumer should wake up and process it.
	bw.Push(&testReq{id: 1, sleep: 10 * time.Millisecond, worked: &worked})

	// Wait long enough for the item to be consumed.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&worked))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bw.GraceFullStop(ctx)
}

// TestGracefulStop uses table-driven sub-tests to verify different
// GraceFullStop scenarios: full drain, concurrent push during stop, and
// idempotency under repeated/concurrent stop calls.
func TestGracefulStop(t *testing.T) {
	tests := []struct {
		name                string
		limit               int64
		initialItems        int
		itemSleep           time.Duration
		concurrentPush      int // items pushed concurrently during stop
		extraStopCalls      int // additional sequential GraceFullStop calls
		concurrentStopCalls int // additional GraceFullStop calls fired in parallel
		stopTimeout         time.Duration
		minWorkedAssert     int32 // minimum number of items that must be processed
		exactWorked         bool  // if true, assert exact match instead of >=
	}{
		{
			name:            "drains all items before returning",
			limit:           10,
			initialItems:    20,
			itemSleep:       50 * time.Millisecond,
			stopTimeout:     10 * time.Second,
			minWorkedAssert: 20,
			exactWorked:     true,
		},
		{
			name:            "concurrent push during stop completes without race",
			limit:           5,
			initialItems:    10,
			itemSleep:       20 * time.Millisecond,
			concurrentPush:  10,
			stopTimeout:     10 * time.Second,
			minWorkedAssert: 10, // at least initial batch
		},
		{
			name:                "repeated and concurrent stop calls are idempotent",
			limit:               1,
			stopTimeout:         time.Second,
			extraStopCalls:      2,
			concurrentStopCalls: 4,
			exactWorked:         true, // no items pushed, expect 0 worked
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			worked := int32(0)
			bw := New(&Options{Limit: tc.limit})

			for i := 0; i < tc.initialItems; i++ {
				bw.Push(&testReq{id: i, sleep: tc.itemSleep, worked: &worked})
			}

			var pushWg sync.WaitGroup
			if tc.concurrentPush > 0 {
				pushWg.Add(1)
				go func() {
					defer pushWg.Done()
					for i := tc.initialItems; i < tc.initialItems+tc.concurrentPush; i++ {
						bw.Push(&testReq{id: i, sleep: 10 * time.Millisecond, worked: &worked})
						time.Sleep(5 * time.Millisecond)
					}
				}()
				// Give the concurrent pusher a head start.
				time.Sleep(30 * time.Millisecond)
			}

			stopOnce := func() {
				ctx, cancel := context.WithTimeout(context.Background(), tc.stopTimeout)
				defer cancel()
				bw.GraceFullStop(ctx)
			}

			assert.NotPanics(t, stopOnce, "first GraceFullStop must not panic")

			for i := 0; i < tc.extraStopCalls; i++ {
				assert.NotPanics(t, stopOnce, "repeated GraceFullStop must be idempotent")
			}

			if tc.concurrentStopCalls > 0 {
				var stopWg sync.WaitGroup
				for i := 0; i < tc.concurrentStopCalls; i++ {
					stopWg.Add(1)
					go func() {
						defer stopWg.Done()
						stopOnce()
					}()
				}
				stopWg.Wait()
			}

			pushWg.Wait()

			got := atomic.LoadInt32(&worked)
			if tc.exactWorked {
				assert.Equal(t, tc.minWorkedAssert, got)
				assert.Equal(t, 0, bw.Len())
				assert.Equal(t, int32(0), bw.Workings())
			} else {
				assert.GreaterOrEqual(t, got, tc.minWorkedAssert)
			}
		})
	}
}

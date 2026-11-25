// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package bufferqueue

import (
	"container/heap"
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

func shortScale(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}

func TestQueue(t *testing.T) {
	worked := int32(0)
	bw := New(&Options{Limit: 1})
	testnum := 10
	sleep := time.Duration(shortScale(1000, 100)) * time.Millisecond
	for i := 1; i <= testnum; i++ {
		bw.Push(&testReq{id: i, sleep: sleep, worked: &worked})
	}
	waittime := time.Duration(testnum)*sleep + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), waittime)
	defer cancel()
	bw.GraceFullStop(ctx)
	assert.Equal(t, int32(testnum), worked)
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
	testnum := shortScale(3000, 300)
	for value, priority := range rand.Perm(testnum) {
		item := &Item{
			value:    value,
			priority: int64(priority),
		}
		heap.Push(&sortq, item)
	}

	previous := heap.Pop(&sortq).(*Item).priority
	for sortq.Len() > 0 {
		current := heap.Pop(&sortq).(*Item).priority
		assert.LessOrEqual(t, previous, current)
		previous = current
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
	testnum := shortScale(3000, 300)
	bw := New(&Options{Limit: 5})
	for i := 1; i <= testnum; i++ {
		randt := time.Duration(float64(rand.Intn(5)) * (1 + 0.8*rand.Float64()))
		t := time.Now().Add(-randt)
		item := &Item{
			value:    &testReqWithTimestamp{id: i, sleep: 5 * time.Millisecond, worked: &worked, createTime: t, dealQ: dealQ},
			priority: t.UnixNano(),
		}
		bw.PushItem(item)
		time.Sleep(time.Millisecond * 10)
	}

	waittime := time.Duration(testnum*10) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), waittime)
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(time.Second)
				t.Logf("workings:%d", bw.Workings())
			}
			if atomic.LoadInt32(&worked) == int32(testnum) {
				cancel()
				return
			}
		}
	}()

	bw.GraceFullStop(ctx)
	assert.Equal(t, int32(testnum), worked)
	assert.Equal(t, int32(0), bw.Workings())
	assert.Equal(t, int(testnum), len(dealQ.list))
}

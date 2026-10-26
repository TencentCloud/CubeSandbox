// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package semaphore

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
)

func TestWeighted(t *testing.T) {
	sem := NewWeighted(10)
	for i := 0; i < 10; i++ {
		assert.True(t, sem.TryAcquire(1))
	}
}
func TestSetLimitUp(t *testing.T) {
	sem := NewWeighted(10)
	for i := 0; i < 10; i++ {
		assert.True(t, sem.TryAcquire(1))
	}
	sem.SetLimit(10 + 3)
	assert.True(t, sem.TryAcquire(1))
	assert.True(t, sem.TryAcquire(1))
	assert.True(t, sem.TryAcquire(1))
	assert.False(t, sem.TryAcquire(1))
	sem.Release(3)
	assert.True(t, sem.TryAcquire(1))
	assert.True(t, sem.TryAcquire(1))
	assert.True(t, sem.TryAcquire(1))
}

func TestSetLimitDown(t *testing.T) {
	sem := NewWeighted(10)
	for i := 0; i < 10; i++ {
		assert.True(t, sem.TryAcquire(1))
	}
	sem.SetLimit(7)
	assert.False(t, sem.TryAcquire(1))
	sem.Release(3)
	assert.False(t, sem.TryAcquire(1))
	sem.Release(1)
	assert.True(t, sem.TryAcquire(1))
	assert.False(t, sem.TryAcquire(1))
}

func TestSetLimitConurrent(t *testing.T) {
	sem := NewWeighted(100)
	worked := int32(0)
	job := func(t time.Duration) {
		defer atomic.AddInt32(&worked, 1)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := sem.Acquire(ctx, 1); err == nil {
				cancel()
				break
			}
			cancel()
		}
		time.Sleep(t)
		sem.Release(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	reqCnt := int32(0)
	for {
		reqCnt++
		recov.GoWithWaitGroup(&wg, func() {
			job(time.Duration(rand.Intn(1000)) * time.Millisecond)
		})
		if reqCnt%2 == 0 {
			size := int64(rand.Intn(100))
			if size < 10 {
				size = 10
			}
			sem.SetLimit(size)
		}
		select {
		case <-ctx.Done():
			goto end
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
end:
	wg.Wait()
	assert.Equal(t, worked, reqCnt)
}

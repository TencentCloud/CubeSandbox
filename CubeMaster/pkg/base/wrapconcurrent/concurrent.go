// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package wrapconcurrent provides a concurrent handle that manages concurrency for tasks.
package wrapconcurrent

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/semaphore"
)

type ConcurrentHandle struct {
	maxConcurrent  int64
	maxRetries     int64
	loopMaxRetries int64
	limiter        *semaphore.Weighted
}

func (c *ConcurrentHandle) SetMaxRetry(n int64) {
	c.maxRetries = n
}

func (c *ConcurrentHandle) MaxRetry() int64 {
	return c.maxRetries
}

func (c *ConcurrentHandle) SetLoopMaxRetry(n int64) {
	c.loopMaxRetries = n
}

func (c *ConcurrentHandle) LoopMaxRetry() int64 {
	return c.loopMaxRetries
}

func (c *ConcurrentHandle) SetLimiter(n int64) {
	if c.limiter == nil {
		if n > 0 {
			c.maxConcurrent = n
			c.limiter = semaphore.NewWeighted(n)
		}
	} else {
		if n > 0 {
			c.limiter.SetLimit(n)
		}
	}
}

func (c *ConcurrentHandle) Acquire(ctx context.Context) error {
	if c.limiter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return c.limiter.Acquire(ctx, 1)
}

func (c *ConcurrentHandle) Release() {
	if c.limiter == nil {
		return
	}
	c.limiter.Release(1)
}

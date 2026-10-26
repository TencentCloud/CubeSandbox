// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package semaphore

import (
	"context"
	"sync/atomic"
)

type Limiter struct {
	sem  *Weighted
	req  atomic.Int64
	peak peakRecorder
}

func NewLimiter(limit int64) *Limiter {
	l := Limiter{
		peak: peakRecorder{
			max: limit,
		},
	}
	if limit > 0 {
		l.sem = NewWeighted(limit)
	}
	return &l
}

func (l *Limiter) SetLimit(limit int64) {
	l.peak.SetMax(limit)
	if l.sem != nil {
		l.sem.Resize(limit)
	}
}

func (l *Limiter) Limit() int64 {
	if l.sem == nil {
		return -1
	}
	return l.sem.Size()
}

func (l *Limiter) TryAcquire() (success bool) {
	var cur int64
	defer func() {
		if success {
			l.peak.Record(cur)
		}
	}()

	if l.sem == nil {
		cur = l.req.Add(1)
		return true
	}
	cur, success = l.sem.TryAcquire(1)
	return
}

func (l *Limiter) Release() {
	if l.sem == nil {
		l.req.Add(-1)
		return
	}
	l.sem.Release(1)
}

func (l *Limiter) Acquire(ctx context.Context) error {
	if l.sem == nil {
		return nil
	}
	return l.sem.Acquire(ctx, 1)
}

func (l *Limiter) Current() int64 {
	if l.sem == nil {
		return l.req.Load()
	}
	return l.sem.GetCurrent()
}

func (l *Limiter) Peak() int64 {
	return l.peak.Get()
}

type peakRecorder struct {
	max int64
	cur int64
}

func (m *peakRecorder) Record(cur int64) {
	for i := 0; i < 5; i++ {
		mcur := m.cur
		if cur > mcur && cur <= m.max {
			if atomic.CompareAndSwapInt64(&m.cur, mcur, cur) {
				return
			}
		} else {
			return
		}
	}
}

func (m *peakRecorder) SetMax(max int64) {
	atomic.StoreInt64(&m.max, max)
	atomic.StoreInt64(&m.cur, 0)
}

func (m *peakRecorder) Get() int64 {
	max := atomic.LoadInt64(&m.cur)
	atomic.StoreInt64(&m.cur, 0)
	return max
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package multilock is a package for multi-lock.
package multilock

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
)

type Options struct {
	CheckInterval   time.Duration
	ExpiredInSecond int64
}

func NewMultiLockOptions() *Options {
	return &Options{
		CheckInterval:   10 * time.Second,
		ExpiredInSecond: 3600,
	}
}

type RWLock interface {
	Lock()
	Unlock()
	RLock()
	RUnlock()
	LockAt()
	UnlockAt()
}

type MultiLock struct {
	*sync.Map
	options *Options
}

func NewMultiLock(options *Options) *MultiLock {
	if options == nil {
		options = NewMultiLockOptions()
	}
	m := &MultiLock{
		Map:     new(sync.Map),
		options: options,
	}
	m.loop()
	return m
}

func (m *MultiLock) Get(key string) RWLock {
	l, ok := m.Load(key)
	if ok {
		lock := l.(*rwLock)
		return lock
	}

	newL := &rwLock{
		&sync.RWMutex{},
		key,
		time.Now().Unix(),
	}

	l, _ = m.LoadOrStore(key, newL)
	return l.(*rwLock)
}

func (m *MultiLock) loop() {
	recov.GoWithRecover(func() {

		ticker := time.NewTicker(m.options.CheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			m.Range(func(key, value any) bool {
				if value == nil {
					return true
				}
				rwl, ok := value.(*rwLock)
				if !ok {
					return true
				}

				rwl.RLock()
				expired := false
				if (time.Now().Unix() - atomic.LoadInt64(&rwl.updateAt)) > m.options.ExpiredInSecond {
					expired = true
				}
				rwl.RUnlock()
				if expired {
					rwl.Lock()
					if (time.Now().Unix() - atomic.LoadInt64(&rwl.updateAt)) > m.options.ExpiredInSecond {
						m.Delete(key)
					}
					rwl.Unlock()
				}
				return true
			})
		}
	})
}

type rwLock struct {
	*sync.RWMutex
	key      string
	updateAt int64
}

func (m *rwLock) LockAt() {
	m.RLock()
	atomic.StoreInt64(&m.updateAt, time.Now().Unix())
}

func (m *rwLock) UnlockAt() {
	m.RUnlock()
}

type lockKey struct{}

func WithLockContext(ctx context.Context, lock RWLock) context.Context {
	return context.WithValue(ctx, lockKey{}, lock)
}

func GetLockFromContext(ctx context.Context) RWLock {
	l := ctx.Value(lockKey{})
	if l == nil {

		return &rwLock{RWMutex: &sync.RWMutex{}}
	}
	return ctx.Value(lockKey{}).(RWLock)
}

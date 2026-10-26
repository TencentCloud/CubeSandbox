// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multilock

import (
	"sync"
	"sync/atomic"
)

type countRefLock struct {
	mu    sync.Mutex
	count int32
}

type RefCountLock struct {
	activeLocks *sync.Map
}

func NewRefCountLock() *RefCountLock {
	return &RefCountLock{
		activeLocks: &sync.Map{},
	}
}

func (r *RefCountLock) Lock(key string) {
	lockObj, _ := r.activeLocks.LoadOrStore(key, &countRefLock{})
	lock := lockObj.(*countRefLock)
	atomic.AddInt32(&lock.count, 1)
	lock.mu.Lock()
}

func (r *RefCountLock) Unlock(key string) {
	lockObj, _ := r.activeLocks.LoadOrStore(key, &countRefLock{})
	lock := lockObj.(*countRefLock)
	lock.mu.Unlock()
	if atomic.AddInt32(&lock.count, -1) <= 0 {
		r.activeLocks.Delete(key)
	}
}

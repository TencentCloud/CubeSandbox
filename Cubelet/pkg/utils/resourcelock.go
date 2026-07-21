// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"sync"
	"time"
)

const resourceLockPollInterval = 5 * time.Millisecond

type ResMutex struct {
	mtx   *sync.Mutex
	count int
}

type ResourceLocks struct {
	mutex sync.Mutex
	locks map[string]*ResMutex
}

func NewResourceLocks() *ResourceLocks {
	return &ResourceLocks{
		mutex: sync.Mutex{},
		locks: make(map[string]*ResMutex),
	}
}

func (r *ResourceLocks) retain(resource string) *ResMutex {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	l, ok := r.locks[resource]
	if ok {
		l.count++
	} else {
		r.locks[resource] = &ResMutex{
			mtx:   &sync.Mutex{},
			count: 1,
		}
		l, _ = r.locks[resource]
	}
	return l
}

func (r *ResourceLocks) release(resource string, l *ResMutex) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	current, ok := r.locks[resource]
	if !ok || current != l {
		return
	}
	if l.count == 1 {
		delete(r.locks, resource)
		return
	}
	l.count--
}

func (r *ResourceLocks) unlock(resource string, l *ResMutex) func() {
	return func() {
		l.mtx.Unlock()
		r.release(resource, l)
	}
}

func (r *ResourceLocks) Lock(resource string) func() {
	l := r.retain(resource)
	l.mtx.Lock()
	return r.unlock(resource, l)
}

// LockContext waits for a resource lock until ctx expires. It uses TryLock
// polling because sync.Mutex has no context-aware wait primitive; the retained
// reference is released on cancellation so a timed-out waiter cannot leak a
// lock entry after the current holder exits.
func (r *ResourceLocks) LockContext(ctx context.Context, resource string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l := r.retain(resource)
	ticker := time.NewTicker(resourceLockPollInterval)
	defer ticker.Stop()

	for {
		if l.mtx.TryLock() {
			if err := ctx.Err(); err != nil {
				l.mtx.Unlock()
				r.release(resource, l)
				return nil, err
			}
			return r.unlock(resource, l), nil
		}

		select {
		case <-ctx.Done():
			r.release(resource, l)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *ResourceLocks) Len() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.locks)
}

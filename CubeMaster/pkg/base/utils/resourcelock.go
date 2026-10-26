// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"sync"
)

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

func (r *ResourceLocks) Lock(resource string) func() {
	r.mutex.Lock()

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

	r.mutex.Unlock()

	l.mtx.Lock()

	return func() {
		l.mtx.Unlock()

		r.mutex.Lock()
		defer r.mutex.Unlock()

		l, ok := r.locks[resource]
		if ok {
			if l.count == 1 {
				delete(r.locks, resource)
				return
			}
			l.count--
		}
	}
}

func (r *ResourceLocks) Len() int {
	return len(r.locks)
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package semaphore implements a weighted semaphore.
package semaphore

import (
	"container/list"
	"context"
	"sync"
)

type waiter struct {
	n     int64
	ready chan<- struct{}
}

func NewWeighted(n int64) *Weighted {
	w := &Weighted{size: n}
	return w
}

type Weighted struct {
	size              int64
	cur               int64
	mu                sync.Mutex
	waiters           list.List
	impossibleWaiters list.List
}

func (s *Weighted) Acquire(ctx context.Context, n int64) error {
	s.mu.Lock()
	if s.size-s.cur >= n && s.waiters.Len() == 0 {
		s.cur += n
		s.mu.Unlock()
		return nil
	}

	var waiterList = &s.waiters

	if n > s.size {

		waiterList = &s.impossibleWaiters
	}

	ready := make(chan struct{})
	w := waiter{n: n, ready: ready}
	elem := waiterList.PushBack(w)
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		err := ctx.Err()
		s.mu.Lock()
		select {
		case <-ready:

			err = nil
		default:
			waiterList.Remove(elem)
		}
		s.mu.Unlock()
		return err

	case <-ready:
		return nil
	}
}

func (s *Weighted) TryAcquire(n int64) bool {
	s.mu.Lock()
	success := s.size-s.cur >= n && s.waiters.Len() == 0
	if success {
		s.cur += n
	}
	s.mu.Unlock()
	return success
}

func (s *Weighted) Release(n int64) {
	s.mu.Lock()
	s.cur -= n
	if s.cur < 0 {
		s.mu.Unlock()
		panic("semaphore: bad release")
	}
	for {
		next := s.waiters.Front()
		if next == nil {
			break
		}

		w := next.Value.(waiter)
		if s.size-s.cur < w.n {

			break
		}

		s.cur += w.n
		s.waiters.Remove(next)
		close(w.ready)
	}
	s.mu.Unlock()
}

func (s *Weighted) SetLimit(n int64) {
	s.mu.Lock()
	if s.size == n {
		s.mu.Unlock()
		return
	}
	s.size = n
	if s.size < 0 {
		s.mu.Unlock()
		panic("semaphore: bad resize")
	}

	element := s.impossibleWaiters.Front()
	for {
		if element == nil {
			break
		}

		w := element.Value.(waiter)
		if s.size < w.n {

			element = element.Next()
			continue
		}

		s.waiters.PushBack(w)
		toRemove := element
		element = element.Next()
		s.impossibleWaiters.Remove(toRemove)

	}

	element = s.waiters.Front()
	for {
		if element == nil {
			break
		}

		w := element.Value.(waiter)
		if s.size >= w.n {

			element = element.Next()
			continue
		}

		s.impossibleWaiters.PushBack(w)
		toRemove := element
		element = element.Next()
		s.waiters.Remove(toRemove)
	}

	for {
		next := s.waiters.Front()
		if next == nil {
			break
		}

		w := next.Value.(waiter)
		if s.size-s.cur < w.n {

			break
		}

		s.cur += w.n
		s.waiters.Remove(next)
		close(w.ready)
	}
	s.mu.Unlock()
}

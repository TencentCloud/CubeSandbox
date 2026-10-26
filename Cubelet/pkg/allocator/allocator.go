// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package allocator

import (
	"errors"
	"sync"
)

var (
	ErrAllocated = errors.New("already allocated")

	ErrOutOfRange = errors.New("out of range")

	ErrExhausted = errors.New("resource exhausted")
)

type Allocator[T comparable] interface {
	Assign(T) error
	Allocate(onExhaustedCallback func() error) (T, error)
	Release(T)
	Has(T) bool
	All() []T
}

type Range[T comparable] interface {
	Contains(T) bool
	GetIter() RangeIterator[T]
	Cap() int
	Expand() (T, error)
}

type RangeIterator[T comparable] interface {
	Get() T
	Next() *T
}

type allocator[T comparable] struct {
	sync.RWMutex
	ranger Range[T]
	store  map[T]struct{}
}

func NewAllocator[T comparable](ranger Range[T]) *allocator[T] {
	return &allocator[T]{
		ranger: ranger,
		store:  make(map[T]struct{}),
	}
}

func (a *allocator[T]) Assign(id T) error {
	a.Lock()
	defer a.Unlock()

	return a.assign(id)
}

func (a *allocator[T]) assign(id T) error {
	if !a.ranger.Contains(id) {
		return ErrOutOfRange
	}
	if a.has(id) {
		return ErrAllocated
	}
	a.store[id] = struct{}{}
	return nil
}

func (a *allocator[T]) Allocate(onExhaustedCallback func() error) (T, error) {
	a.Lock()
	defer a.Unlock()

	t, err := a.allocate()
	if err == nil {
		return t, err
	}
	if onExhaustedCallback != nil && errors.Is(err, ErrExhausted) {
		if err1 := onExhaustedCallback(); err1 != nil {
			return t, err1
		}

		return a.allocate()
	}

	return t, err
}

func (a *allocator[T]) allocate() (T, error) {
	if len(a.store) >= a.ranger.Cap() {
		var noop T
		return noop, ErrExhausted
	}

	ri := a.ranger.GetIter()

	for {
		id := ri.Get()
		if a.assign(id) == nil {
			return id, nil
		}
		if ri.Next() == nil {
			break
		}
	}

	var noop T
	return noop, ErrExhausted
}

func (a *allocator[T]) Release(id T) {
	a.Lock()
	defer a.Unlock()

	delete(a.store, id)
}

func (a *allocator[T]) Has(id T) bool {
	a.RLock()
	defer a.RUnlock()

	return a.has(id)
}

func (a *allocator[T]) has(id T) bool {
	if _, exist := a.store[id]; exist {
		return true
	}
	return false
}

func (a *allocator[T]) All() []T {
	a.RLock()
	defer a.RUnlock()

	var ids []T
	for id := range a.store {
		ids = append(ids, id)
	}
	return ids
}

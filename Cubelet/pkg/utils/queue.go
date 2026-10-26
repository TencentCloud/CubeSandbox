// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"sync/atomic"
	"unsafe"
)

type Queue[T any] struct {
	head unsafe.Pointer
	tail unsafe.Pointer
	len  uint64
}

func NewQueue[T any]() *Queue[T] {
	head := item[T]{}

	return &Queue[T]{
		tail: unsafe.Pointer(&head),
		head: unsafe.Pointer(&head),
	}
}

func (q *Queue[T]) Enqueue(v *T) {
	i := &item[T]{next: nil, v: v}
	var last, lastnext *item[T]
	for {
		last = loadItem[T](&q.tail)
		lastnext = loadItem[T](&last.next)
		if loadItem[T](&q.tail) == last {
			if lastnext == nil {
				if casItem(&last.next, lastnext, i) {
					casItem(&q.tail, last, i)
					atomic.AddUint64(&q.len, 1)

					return
				}
			} else {
				casItem(&q.tail, last, lastnext)
			}
		}
	}
}

func (q *Queue[T]) Dequeue() *T {
	var first, last, firstNext *item[T]
	for {
		first = loadItem[T](&q.head)
		last = loadItem[T](&q.tail)
		firstNext = loadItem[T](&first.next)
		if first == loadItem[T](&q.head) {
			if first == last {
				if firstNext == nil {
					return nil
				}
				casItem(&q.tail, last, firstNext)
			} else {
				v := firstNext.v
				if casItem(&q.head, first, firstNext) {
					atomic.AddUint64(&q.len, ^uint64(0))

					return v
				}
			}
		}
	}
}

func (q *Queue[T]) Length() int {
	return int(atomic.LoadUint64(&q.len))
}

type Less func(a, b interface{}) bool

type item[T any] struct {
	next unsafe.Pointer
	v    *T
}

func loadItem[T any](p *unsafe.Pointer) *item[T] {
	return (*item[T])(atomic.LoadPointer(p))
}

func casItem[T any](p *unsafe.Pointer, old, new *item[T]) bool {
	return atomic.CompareAndSwapPointer(p, unsafe.Pointer(old), unsafe.Pointer(new))
}

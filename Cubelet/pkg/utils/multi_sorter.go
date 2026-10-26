// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"sort"
)

type lessFunc[T any] func(p1, p2 *T) bool

type multiSorter[T any] struct {
	changes []T
	less    []lessFunc[T]
}

func (ms *multiSorter[T]) Sort(changes []T) {
	ms.changes = changes
	sort.Sort(ms)
}

func OrderedBy[T any](less ...lessFunc[T]) *multiSorter[T] {
	return &multiSorter[T]{
		less: less,
	}
}

func (ms *multiSorter[T]) Len() int {
	return len(ms.changes)
}

func (ms *multiSorter[T]) Swap(i, j int) {
	ms.changes[i], ms.changes[j] = ms.changes[j], ms.changes[i]
}

func (ms *multiSorter[T]) Less(i, j int) bool {
	p, q := &ms.changes[i], &ms.changes[j]

	var k int
	for k = 0; k < len(ms.less)-1; k++ {
		less := ms.less[k]
		switch {
		case less(p, q):

			return true
		case less(q, p):

			return false
		}

	}

	return ms.less[k](p, q)
}

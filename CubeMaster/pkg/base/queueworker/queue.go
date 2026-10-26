// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package queueworker provides a simple queue worker.
package queueworker

import (
	"errors"
)

var (
	ErrFull = errors.New("queue is full")
)

type Queue interface {
	Push(e interface{}) error

	Pop() (interface{}, error)

	BPush(e interface{})

	BPop() (interface{}, bool)

	QueueCh() chan interface{}

	Len() int

	Close()
}

type queue struct {
	queueCh chan interface{}
}

func NewQueue(size int) Queue {
	return &queue{
		queueCh: make(chan interface{}, size),
	}
}

func (q *queue) Push(t interface{}) error {
	select {
	case q.queueCh <- t:
		return nil
	default:
		return ErrFull
	}
}

func (q *queue) Pop() (interface{}, error) {
	select {
	case t := <-q.queueCh:
		return t, nil
	default:
		return nil, errors.New("queue is empty")
	}
}

func (q *queue) BPush(t interface{}) {
	q.queueCh <- t
}

func (q *queue) BPop() (t interface{}, ok bool) {
	t, ok = <-q.queueCh
	return
}

func (q *queue) QueueCh() chan interface{} {
	return q.queueCh
}

func (q *queue) Len() int {
	return len(q.queueCh)
}

func (q *queue) Close() {
	close(q.queueCh)
}

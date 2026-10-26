// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package queueworker

import (
	"context"
	"fmt"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
)

type WorkHandler func(interface{}) error

type QueueWorker interface {
	Stop()

	GraceFullStop(ctx context.Context)

	Errors() chan error

	Push(interface{}) error

	Pop() (interface{}, error)

	BPop() (interface{}, bool)

	QueueCh() chan interface{}

	Len() int
}

type Options struct {
	QueueSize int
	WorkerNum int
}

type queueWorker struct {
	opt     *Options
	errCh   chan error
	stopped chan struct{}

	Queue Queue
}

func NewQueueWorker(opt *Options, wh WorkHandler) QueueWorker {
	qw := &queueWorker{
		opt:     opt,
		errCh:   make(chan error, 1),
		Queue:   NewQueue(opt.QueueSize),
		stopped: make(chan struct{}, 1),
	}
	go qw.start(wh)
	return qw
}

func (qw *queueWorker) start(wh WorkHandler) {
	var wg sync.WaitGroup
	for i := 0; i < qw.opt.WorkerNum; i++ {
		recov.GoWithWaitGroup(&wg, func() {
			for {
				data, ok := qw.BPop()
				if ok {
					if err := wh(data); err != nil {
						select {
						case qw.Errors() <- err:
						default:
						}
					}
				} else {

					return
				}
			}
		}, func(panicError interface{}) {
			select {
			case qw.Errors() <- fmt.Errorf("panic: %v", panicError):
			default:
			}
		})
	}
	wg.Wait()
	qw.stopped <- struct{}{}
}

func (qw *queueWorker) Errors() chan error {
	return qw.errCh
}

func (qw *queueWorker) Stop() {
	qw.Queue.Close()
}

func (qw *queueWorker) GraceFullStop(ctx context.Context) {
	qw.Queue.Close()
	select {
	case <-qw.stopped:
		return

	case <-ctx.Done():
		return
	}
}

func (qw *queueWorker) Push(t interface{}) error {
	return qw.Queue.Push(t)
}

func (qw *queueWorker) Pop() (interface{}, error) {
	return qw.Queue.Pop()
}

func (qw *queueWorker) BPop() (interface{}, bool) {
	return qw.Queue.BPop()
}

func (qw *queueWorker) QueueCh() chan interface{} {
	return qw.Queue.QueueCh()
}

func (qw *queueWorker) Len() int {
	return qw.Queue.Len()
}

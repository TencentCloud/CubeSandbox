// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package workpool implements a worker pool
package workpool

import (
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
)

type WorkerPool interface {
	Exec(task func())
	Close()
}

type workerPool struct {
	semCh chan struct{}
	sync.Once
}

func NewWorkerPool(size int) WorkerPool {
	return &workerPool{
		semCh: make(chan struct{}, size),
	}
}

func (p *workerPool) Exec(task func()) {
	select {
	case p.semCh <- struct{}{}:
		recov.GoWithRecover(func() {
			p.do(task)
		})
	}
}

func (p *workerPool) do(task func()) {
	defer func() {
		r := recover()
		<-p.semCh
		if r != any(nil) {
			panic(r)
		}
	}()
	task()
}

func (p *workerPool) Close() {
	p.Do(func() {
		close(p.semCh)
	})
}

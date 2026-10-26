// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package task provides the task module
package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/queueworker"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
)

type OpType string

const (
	DestroySandbox OpType = "Destroy"
	CreateImage    OpType = "CreateImage"
	DeleteImage    OpType = "DeleteImage"
	UpdateSandbox  OpType = "UpdateSandbox"
)

type Task struct {
	BaseInfo
	Ctx       context.Context
	RequestId string
	Request   any
	CallEp    string
	TaskType  OpType

	DescribeTaskID string

	retryTimes int64
	loopRetry  bool
	delay      time.Duration
	start      time.Time
}

type BaseInfo struct {
	InstanceType string
	SandboxID    string
	InstanceID   string
}

func (b BaseInfo) InsType() string {
	return b.InstanceType
}

func (b BaseInfo) SandBoxID() string {
	return b.SandboxID
}

func (b BaseInfo) InsID() string {
	return b.InstanceID
}

type localTask struct {
	handles map[OpType]taskHandler

	asyncTask    queueworker.QueueWorker
	initialDelay time.Duration
	stop         chan struct{}
	ctx          context.Context
}

var l *localTask

func InitTask(ctx context.Context, cfg *config.Config) {
	l = &localTask{
		ctx:          ctx,
		initialDelay: time.Duration(1) * time.Second,
		stop:         make(chan struct{}),
	}

	l.handles = make(map[OpType]taskHandler)
	l.handles[DestroySandbox] = &DestroySandboxTaskHandler{}
	l.handles[CreateImage] = &CreateImageTaskHandler{}
	l.handles[DeleteImage] = &DeleteImageTaskHandler{}
	l.handles[UpdateSandbox] = &UpdateSandboxTaskHandler{}

	for k, h := range l.handles {
		h.SetMaxRetry(cfg.CubeletConf.MaxRetries)
		h.SetLoopMaxRetry(cfg.CubeletConf.LoopMaxRetries)
		if v, ok := cfg.CubeletConf.AsyncFlows[string(k)]; ok {
			if v.MaxConcurrent > 0 {
				h.SetLimiter(v.MaxConcurrent)
				fmt.Printf("InitTask %s AddConcurrent %d\n", k, v.MaxConcurrent)
			}
			if v.MaxRetries > 0 {
				h.SetMaxRetry(v.MaxRetries)
				fmt.Printf("InitTask %s SetMaxRetry %d\n", k, v.MaxRetries)
			}
			if v.LoopMaxRetries > 0 {
				h.SetLoopMaxRetry(v.LoopMaxRetries)
				fmt.Printf("InitTask %s SetLoopMaxRetry %d\n", k, v.LoopMaxRetries)
			}
		}
	}

	workerNum := cfg.Common.AsyncTaskWorkerNum
	if workerNum < len(l.handles) {
		workerNum = len(l.handles)
	}

	l.asyncTask = queueworker.NewQueueWorker(&queueworker.Options{
		QueueSize: cfg.Common.AsyncTaskQueueSize,
		WorkerNum: workerNum,
	}, l.workHandler)

	recov.GoWithRecover(l.reportMetric)

	config.AppendConfigWatcher(l)
}

func (l *localTask) OnEvent(config *config.Config) {

	for k, h := range l.handles {
		if k == DestroySandbox {

			continue
		}
		h.SetMaxRetry(config.CubeletConf.MaxRetries)
		h.SetLoopMaxRetry(config.CubeletConf.LoopMaxRetries)
		if v, ok := config.CubeletConf.AsyncFlows[string(k)]; ok {
			if v.MaxConcurrent > 0 {
				h.SetLimiter(v.MaxConcurrent)
				fmt.Printf("OnEvent %s AddConcurrent %d\n", k, v.MaxConcurrent)
			}
			if v.MaxRetries > 0 {
				h.SetMaxRetry(v.MaxRetries)
				fmt.Printf("OnEvent %s SetMaxRetry %d\n", k, v.MaxRetries)
			}
			if v.LoopMaxRetries > 0 {
				h.SetLoopMaxRetry(v.LoopMaxRetries)
				fmt.Printf("OnEvent %s SetLoopMaxRetry %d\n", k, v.LoopMaxRetries)
			}
		}
	}
}

func Stop(ctx context.Context) {
	close(l.stop)

	if l.asyncTask != nil {
		l.asyncTask.GraceFullStop(ctx)
	}
}

func AsyncTaskLen() int {
	if l.asyncTask == nil {
		return 0
	}
	return l.asyncTask.Len()
}

func AddAsyncTask(t *Task) error {
	if l.asyncTask == nil {
		return errors.New("task worker is nil")
	}
	return l.asyncTask.Push(t)
}

func SetTaskWorkerConcurrent(action OpType, n int64) {
	if h, ok := l.handles[action]; ok {
		if n > 0 {
			h.SetLimiter(n)
		}
	}
}

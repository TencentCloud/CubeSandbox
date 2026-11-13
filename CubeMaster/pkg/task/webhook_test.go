// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"errors"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/queueworker"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"sync/atomic"
	"testing"
	"time"
)

func TestP4OriginAcrossDestroyQueueRetry(t *testing.T) {
	p := gomonkey.NewPatches()
	defer p.Reset()
	p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, f func(context.Context) error) error {
		return f(ctx)
	})
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return &node.Node{}, true })
	p.ApplyFunc(pausesnap.TryDeletePausedWithCompletion, func(context.Context, string, string, string, *bool) (bool, bool, error) { return false, false, nil })
	p.ApplyFunc(reportMetric, func(*Task, error) {})
	errorcode.InitCubeCodeRetryMap(&config.Config{CubeletConf: &config.CubeletConf{}})
	var calls atomic.Int32
	origins := make(chan webhook.Origin, 2)
	p.ApplyFunc(cubelet.Destroy, func(ctx context.Context, _ string, _ *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		origins <- webhook.OriginFromContext(ctx)
		if calls.Add(1) == 1 {
			return nil, errors.New("retry")
		}
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	h := &DestroySandboxTaskHandler{}
	h.SetMaxRetry(1)
	h.SetLoopMaxRetry(1)
	old := l
	l = &localTask{handles: map[OpType]taskHandler{DestroySandbox: h}}
	defer func() { l = old }()
	done := make(chan struct{})
	l.asyncTask = queueworker.NewQueueWorker(&queueworker.Options{QueueSize: 4, WorkerNum: 1}, func(v interface{}) error {
		e := handleTask(v.(*Task))
		if e == nil {
			close(done)
		}
		return nil
	})
	defer l.asyncTask.GraceFullStop(context.Background())
	// Non-cubebox avoids unrelated proxy cleanup; actual Destroy handler still
	// applies WithoutCancel and retries via the real queue with the same Task.
	job := &Task{Ctx: webhook.WithOrigin(context.Background(), webhook.OriginCreateFailover), TaskType: DestroySandbox, CallEp: "127.0.0.1:1", Request: &cubebox.DestroyCubeSandboxRequest{SandboxID: "sb"}}
	require.NoError(t, AddAsyncTask(job))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("destroy retry timeout")
	}
	require.Equal(t, webhook.OriginCreateFailover, <-origins)
	require.Equal(t, webhook.OriginCreateFailover, <-origins)
	require.Equal(t, int64(1), job.retryTimes)
}

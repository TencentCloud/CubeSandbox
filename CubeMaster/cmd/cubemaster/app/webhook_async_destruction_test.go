// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestAppPublisherRealAsyncDelete(t *testing.T) {
	// InitTask registers process-global watchers; isolate and reclaim all of them
	// by running this test in a child process, with no production scheduler seam.
	if os.Getenv("WEBHOOK_TEST_ASYNC_QUEUE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAppPublisherRealAsyncDelete$", "-test.v")
		cmd.Env = append(os.Environ(), "WEBHOOK_TEST_ASYNC_QUEUE_CHILD=1")
		output, err := cmd.CombinedOutput()
		t.Log(string(output))
		require.NoError(t, err)
		return
	}

	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg := &config.Config{Common: &config.CommonConf{AsyncTaskWorkerNum: 4, AsyncTaskQueueSize: 4, CollectMetricInterval: time.Millisecond}, CubeletConf: &config.CubeletConf{MaxRetries: 1, LoopMaxRetries: 1}, Webhooks: config.DefaultWebhookConf()}
	cfg.Webhooks.Enabled = true
	cfg.Webhooks.AllowHTTP = true
	cfg.Webhooks.AllowedCIDRs = []string{"127.0.0.0/8"}
	cfg.Webhooks.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeKilled}}}
	a := New()
	a.startWebhooks(context.Background(), cfg)
	defer a.webhooks.Shutdown(context.Background())
	defer sandbox.SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	p := gomonkey.NewPatches()
	defer p.Reset()
	p.ApplyFunc(config.GetConfig, func() *config.Config { return cfg })
	p.ApplyFunc(sandbox.ResolveSandboxID, func(_ context.Context, id string) (string, error) { return id, nil })
	p.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache { return &localcache.SandboxCache{HostIP: "127.0.0.1"} })
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return &node.Node{}, true })
	p.ApplyFunc(cubelet.GetCubeletAddr, func(string) string { return "127.0.0.1:1" })
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) { return nil, nil })
	p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	p.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) { return false, nil })
	var cleanupCalls, nodeCalls atomic.Int32
	p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error {
		if cleanupCalls.Add(1) == 1 {
			return errors.New("retry control-plane cleanup after node success")
		}
		return nil
	})
	entered, release := make(chan int32, 2), make(chan struct{})
	p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		entered <- nodeCalls.Add(1)
		<-release
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	taskCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorcode.InitCubeCodeRetryMap(cfg)
	task.InitTask(taskCtx, cfg)
	defer task.Stop(context.Background())
	rsp := sandbox.DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "app-async-delete", InstanceType: "cubebox", KillReason: "request"})
	require.Equal(t, 200, rsp.Ret.RetCode)
	select {
	case n := <-entered:
		require.Equal(t, int32(1), n)
	case <-time.After(3 * time.Second):
		t.Fatal("real queue did not invoke node")
	}
	require.Empty(t, bodies, "enqueue and an in-flight node call are not handler success")
	release <- struct{}{}
	select {
	case n := <-entered:
		require.Equal(t, int32(2), n, "original queue retry must call the node again")
	case <-time.After(4 * time.Second):
		t.Fatal("real queue did not retry after cleanup failure")
	}
	require.Empty(t, bodies, "failed cleanup and retry in flight are not handler success")
	release <- struct{}{}
	select {
	case body := <-bodies:
		require.Equal(t, int32(2), nodeCalls.Load())
		require.Equal(t, int32(2), cleanupCalls.Load())
		require.Contains(t, string(body), "app-async-delete")
		require.Contains(t, string(body), webhook.TypeKilled)
	case <-time.After(10 * time.Second):
		t.Fatal("App did not inject task publisher")
	}
}

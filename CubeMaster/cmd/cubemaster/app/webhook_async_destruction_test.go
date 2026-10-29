// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestP5AppPublisherRealAsyncDelete(t *testing.T) {
	addr := os.Getenv("WEBHOOK_P4_REDIS_ADDR")
	if addr == "" {
		t.Skip("isolated Redis not configured")
	}
	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg := &config.Config{Common: &config.CommonConf{}, Webhooks: config.DefaultWebhookConf()}
	cfg.Webhooks.Enabled = true
	cfg.Webhooks.AllowHTTP = true
	cfg.Webhooks.AllowedCIDRs = []string{"127.0.0.0/8"}
	cfg.Webhooks.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeKilled}}}
	cfg.Webhooks.Store.Redis = &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: 15}
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
	p.ApplyFunc(pausesnap.TryDeletePausedWithCompletion, func(context.Context, string, string, string, *bool) (bool, bool, error) { return false, false, nil })
	p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { return nil })
	p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	var pending *task.Task
	p.ApplyFunc(task.AddAsyncTask, func(job *task.Task) error { pending = job; return nil })
	rsp := sandbox.DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "app-p5-delete", InstanceType: "cubebox", KillReason: "request"})
	require.Equal(t, 200, rsp.Ret.RetCode)
	require.NotNil(t, pending)
	// Deterministic pre-completion publication is separately checked with the
	// BufferedPublisher in the producer barrier test.
	require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(pending.Ctx, pending))
	select {
	case body := <-bodies:
		require.Contains(t, string(body), "app-p5-delete")
		require.Contains(t, string(body), webhook.TypeKilled)
	case <-time.After(10 * time.Second):
		t.Fatal("App did not inject task publisher")
	}
}

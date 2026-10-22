// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestP4AppPublisherRealCreate(t *testing.T) {
	addr := os.Getenv("WEBHOOK_P4_REDIS_ADDR")
	if addr == "" {
		t.Skip("isolated Redis not configured")
	}
	path, e := filepath.Abs("../../../conf.yaml")
	require.NoError(t, e)
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	cfg, e := config.Init()
	require.NoError(t, e)
	cfg.Common.MockCreateDirectHandle = true
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg.Webhooks = config.DefaultWebhookConf()
	cfg.Webhooks.Enabled = true
	cfg.Webhooks.AllowHTTP = true
	cfg.Webhooks.AllowedCIDRs = []string{"127.0.0.0/8"}
	cfg.Webhooks.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated}}}
	cfg.Webhooks.Store.Redis = &config.RedisConf{Nodes: addr, Password: os.Getenv("WEBHOOK_P4_REDIS_PASSWORD"), DbNo: 14}
	a := New()
	a.startWebhooks(context.Background(), cfg)
	defer a.webhooks.Shutdown(context.Background())
	defer sandbox.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	p := gomonkey.NewPatches()
	defer p.Reset()
	n := &node.Node{InsID: "node", IP: "127.0.0.1"}
	p.ApplyFunc(localcache.IncrNodeConcurrent, func(*node.Node) error { return nil })
	p.ApplyFunc(localcache.DecrNodeConcurrent, func(*node.Node) error { return nil })
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return n, true })
	p.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) { return n, true })
	p.ApplyFunc(localcache.SetSandboxProxyMap, func(context.Context, *proxytypes.SandboxProxyMap) error { return nil })
	p.ApplyFunc(sandbox.ConstructCubeletReq, func(context.Context, *types.CreateCubeSandboxReq) (*cubebox.RunCubeSandboxRequest, error) {
		return &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"}, nil
	})
	p.ApplyFunc(cubelet.Create, func(context.Context, string, *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
		return &cubebox.RunCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SandboxID: "app-real-create", SandboxIP: "10.0.0.1"}, nil
	})
	rsp := sandbox.CreateSandbox(context.Background(), &types.CreateCubeSandboxReq{Request: &types.Request{RequestID: "r"}, InsIp: "127.0.0.1", InstanceType: "cubebox", Annotations: map[string]string{constants.AnnotationsDebug: "true"}, Containers: []*types.Container{{Image: &types.ImageSpec{}, Resources: &types.Resource{Cpu: "1", Mem: "1Gi"}}}})
	require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	select {
	case body := <-bodies:
		require.Contains(t, string(body), "app-real-create")
	case <-time.After(8 * time.Second):
		t.Fatal("App did not inject runtime into create")
	}
}

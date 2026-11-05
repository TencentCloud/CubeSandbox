// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/cube"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAppPublisherPublicCreate(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "invalid"} {
		t.Run(mode, func(t *testing.T) { appPublisherPublicCreate(t, mode) })
	}
}

func appPublisherPublicCreate(t *testing.T, mode string) {
	path, e := filepath.Abs("../../../conf.yaml")
	require.NoError(t, e)
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	cfg, e := config.Init()
	require.NoError(t, e)
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(204)
	}))
	defer srv.Close()
	cfg.Webhooks = config.DefaultWebhookConf()
	cfg.Webhooks.Enabled = mode != "disabled"
	cfg.Webhooks.Invalid = mode == "invalid"
	cfg.Webhooks.AllowHTTP = true
	cfg.Webhooks.AllowedCIDRs = []string{"127.0.0.0/8"}
	cfg.Webhooks.Subscriptions = []config.WebhookSubscription{{ID: uuid.NewString(), Enabled: true, URL: srv.URL, Events: []string{webhook.TypeCreated}}}
	a := New()
	a.startWebhooks(context.Background(), cfg)
	defer a.webhooks.Shutdown(context.Background())
	defer sandbox.SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	defer templatecenter.SetWebhookPublisher(nil)
	before := webhookCounter(t, "capture", "accepted")
	p := gomonkey.NewPatches()
	defer p.Reset()
	if mode != "enabled" {
		p.ApplyFunc(webhook.UserMetadata, func(map[string]string) webhook.LifecycleFields { panic("App disabled metadata") })
		p.ApplyFunc(webhook.PrepareLifecycle, func(*types.CreateCubeSandboxReq) (string, webhook.LifecycleFields) { panic("App disabled fields") })
		p.ApplyFunc(webhook.Capture, func(webhook.CaptureInput) (*webhook.CapturedEvent, error) { panic("App disabled Capture") })
	}
	// Exercise App -> Holder -> producer -> Runtime -> HTTP. Business creation
	// has its own tests; do not start scheduler/metrics workers in this wiring test.
	createCalls := 0
	p.ApplyFunc(sandbox.CreateSandbox, func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		createCalls++
		require.NotNil(t, CubeLog.GetTraceInfo(ctx))
		return &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: 200}, SandboxID: "app-create"}
	})
	req := &types.CreateCubeSandboxReq{Volumes: []*types.Volume{{Name: "tmp", VolumeSource: &types.VolumeSource{EmptyDir: &types.EmptyDirVolumeSource{}}}}, Request: &types.Request{RequestID: "r"}, InsIp: "127.0.0.1", InstanceType: "cubebox", Annotations: map[string]string{constants.AnnotationsDebug: "true"}, Containers: []*types.Container{{Image: &types.ImageSpec{}, Resources: &types.Resource{Cpu: "1", Mem: "1Gi"}}}}
	payload, e := json.Marshal(req)
	require.NoError(t, e)
	router := gin.New()
	cube.RegisterCubeRoutes(router.Group("/cube"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/cube/sandbox", bytes.NewReader(payload)).WithContext(CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{})))
	require.Equal(t, 1, createCalls)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "app-create")
	if mode != "enabled" {
		require.Equal(t, before, webhookCounter(t, "capture", "accepted"))
		require.Empty(t, bodies)
		return
	}

	select {
	case body := <-bodies:
		var event struct {
			Type      string `json:"type"`
			SandboxID string `json:"sandbox_id"`
		}
		require.NoError(t, json.Unmarshal(body, &event))
		require.Equal(t, webhook.TypeCreated, event.Type)
		require.Equal(t, "app-create", event.SandboxID)
	case <-time.After(8 * time.Second):
		t.Fatal("App did not inject runtime into create")
	}
}

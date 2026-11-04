// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// Exercise the real CreateSandbox/Wait result arbitration and the public HTTP
// success branch. A successful Cubelet response alone does not decide success.
func TestCreateWebhookCancellationOrder(t *testing.T) {
	for _, kind := range []string{"template", "snapshot"} {
		for _, order := range []string{"cancel-wins", "success-wins"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p, err := webhook.NewBufferedPublisher(4)
				require.NoError(t, err)
				sandbox.SetWebhookPublisher(p)
				task.SetWebhookPublisher(p)
				defer sandbox.SetWebhookPublisher(nil)
				defer task.SetWebhookPublisher(nil)
				oldDeal, oldRun, oldRegister := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn
				defer func() {
					createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn = oldDeal, oldRun, oldRegister
				}()
				patches := gomonkey.NewPatches()
				defer patches.Reset()
				cfg := &config.Config{Common: &config.CommonConf{}, Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{MaxMvmCPU: "64", MaxMvmMemory: "64Gi"}}, CubeletConf: &config.CubeletConf{CreateTimeoutInsec: 30}}
				patches.ApplyFunc(config.GetConfig, func() *config.Config { return cfg })
				n := &node.Node{InsID: "node", IP: "127.0.0.1"}
				patches.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return n, true })
				patches.ApplyFunc(localcache.GetNode, func(string) (*node.Node, bool) { return n, true })
				patches.ApplyFunc(localcache.IncrNodeConcurrent, func(*node.Node) error { return nil })
				patches.ApplyFunc(localcache.DecrNodeConcurrent, func(*node.Node) error { return nil })
				patches.ApplyFunc(cubelet.GetCubeletAddr, func(string) string { return "127.0.0.1:1" })
				patches.ApplyFunc(sandbox.ConstructCubeletReq, func(context.Context, *types.CreateCubeSandboxReq) (*cubebox.RunCubeSandboxRequest, error) {
					return &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"}, nil
				})
				nodeCalls, proxyCalls, refCalls := 0, 0, 0
				patches.ApplyFunc(cubelet.Create, func(context.Context, string, *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
					nodeCalls++
					return &cubebox.RunCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SandboxID: "ordered-create", SandboxIP: "10.0.0.1"}, nil
				})
				entered, release, workerDone, metricDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				patches.ApplyFunc(localcache.SetSandboxProxyMap, func(context.Context, *proxytypes.SandboxProxyMap) error {
					proxyCalls++
					if order == "cancel-wins" {
						close(entered)
						<-release
					}
					return nil
				})
				// Join metrics before releasing late post-processing or resetting
				// patches. The traced App entrypoint remains covered separately.
				patches.ApplyFunc(CubeLog.Trace, func(rt *CubeLog.RequestTrace) {
					if rt.CalleeAction == constants.ActionBufferHandle {
						close(metricDone)
					}
				})
				patches.ApplyFunc(scheduler.AddBufferTask, func(value interface{}, _ string) {
					handle := func() { value.(interface{ Handle() }).Handle(); close(workerDone) }
					if order == "cancel-wins" {
						go handle()
						<-entered
						cancel() // Wait must decide cancellation while proxy work is blocked.
					} else {
						handle()
					}
				})
				var pending *task.Task
				patches.ApplyFunc(task.AddAsyncTask, func(value *task.Task) error { pending = value; return nil })
				createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
					resolved := templateResolveResultFromContext(ctx)
					resolved.TemplateID, resolved.Kind = "trusted-template", kind
					req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = "trusted-template"
					req.Annotations[constants.CubeAnnotationAppSnapshotTemplateAlias] = "trusted-alias"
					return nil
				}
				createSandboxRegisterRuntimeRefFn = func(context.Context, string, string, string, string) error { refCalls++; return nil }
				createSandboxRunFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
					result := sandbox.CreateSandbox(ctx, req)
					<-metricDone
					if order == "cancel-wins" {
						require.Equal(t, int(errorcode.ErrorCode_ClientCancel), result.Ret.RetCode)
						close(release) // Backend now finishes successfully, after Wait returned.
					} else {
						require.Equal(t, int(errorcode.ErrorCode_Success), result.Ret.RetCode)
						require.NoError(t, ctx.Err())
						cancel() // Real business success returned before the outer hook.
					}
					<-workerDone
					return result
				}
				body := `{"requestID":"ordered","ins_ip":"127.0.0.1","annotations":{"com.cube.debug":"true"},"containers":[{"image":{},"resources":{"cpu":"1","mem":"1Gi"}}]}`
				request := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(body)).WithContext(ctx)
				result := createSandbox(request, &CubeLog.RequestTrace{}).(*types.CreateCubeSandboxRes)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.Equal(t, 1, nodeCalls)
				require.Equal(t, 1, proxyCalls)
				if order == "cancel-wins" {
					require.Equal(t, int(errorcode.ErrorCode_ClientCancel), result.Ret.RetCode)
					require.Zero(t, refCalls)
					require.NotNil(t, pending)
					require.Equal(t, webhook.OriginCreateFailover, webhook.OriginFromContext(pending.Ctx))
					// Execute the existing rollback handler, including its notification hook.
					patches.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
						return fn(ctx)
					})
					patches.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) { return false, nil })
					patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
						return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
					})
					patches.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { return nil })
					require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(pending.Ctx, pending))
					require.Empty(t, p.Events(), "cancellation and internal rollback publish no lifecycle success")
					return
				}
				require.Nil(t, pending)
				require.Equal(t, "trusted-alias", result.ExtInfo[constants.CubeAnnotationAppSnapshotTemplateAlias])
				require.Equal(t, map[bool]int{false: 0, true: 1}[kind == "snapshot"], refCalls)
				require.Len(t, p.Events(), 1)
				captured := <-p.Events()
				data, err := webhook.MapToProtocol(captured)
				require.NoError(t, err)
				event, err := webhook.Encode(captured, data)
				require.NoError(t, err)
				want := webhook.TypeCreated
				if kind == "snapshot" {
					want = webhook.TypeResumed
					require.Equal(t, "restore", data.ResumeMode)
				}
				require.Equal(t, want, event.Type())
			})
		}
	}
}

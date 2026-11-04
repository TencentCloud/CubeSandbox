// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/queueworker"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type timeoutFixture struct {
	end int64
	err error
}

func (p timeoutFixture) RefreshTimeout(context.Context, string, int) (int64, error) {
	return p.end, p.err
}
func (p timeoutFixture) LookupEndAt(context.Context, string) (int64, error) { return p.end, p.err }
func readUpdateDestroyEvent(t *testing.T, p *webhook.BufferedPublisher) map[string]any {
	t.Helper()
	select {
	case c := <-p.Events():
		d, e := webhook.MapToProtocol(c)
		require.NoError(t, e)
		ev, e := webhook.Encode(c, d)
		require.NoError(t, e)
		var body map[string]any
		require.NoError(t, json.Unmarshal(ev.Body(), &body))
		return body
	default:
		t.Fatal("missing event")
		return nil
	}
}
func TestTimeoutResponseBoundaryIncludesFallback(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	localcache.SetSandboxCache("sb-timeout", &localcache.SandboxCache{SandboxID: "sb-timeout", HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache("sb-timeout")
	old := getTimeoutProvider()
	defer SetTimeoutProvider(old)
	for _, tc := range []struct {
		name     string
		provider TimeoutProvider
		value    int32
		event    bool
	}{
		{"finite", timeoutFixture{1700000000123, nil}, 5, true},
		{"zero", timeoutFixture{1700000000123, nil}, 0, true},
		{"infinite", timeoutFixture{0, nil}, -1, false},
		{"provider-error-fallback", timeoutFixture{0, errors.New("read")}, 5, true},
		{"missing-provider-fallback", nil, 5, true},
		{"zero-result-fallback", timeoutFixture{0, nil}, 5, true},
		{"invalid", timeoutFixture{1700000000123, nil}, -2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := lifecycleBuffer(t)
			SetTimeoutProvider(tc.provider)
			before := time.Now().UnixMilli()
			rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-timeout", Timeout: tc.value})
			if tc.value >= -1 {
				require.Equal(t, 200, rsp.Ret.RetCode)
			} else {
				require.NotEqual(t, 200, rsp.Ret.RetCode)
			}
			if tc.event {
				require.Positive(t, rsp.EndAt)
				body := readUpdateDestroyEvent(t, p)
				require.Equal(t, "sb-timeout", body["sandbox_id"])
				require.Equal(t, time.UnixMilli(rsp.EndAt).UTC().Format(time.RFC3339), body["event_data"].(map[string]any)["set_timeout"])
				if strings.Contains(tc.name, "fallback") {
					require.GreaterOrEqual(t, rsp.EndAt, before+5000)
				}
			}
			require.Empty(t, p.Events())
		})
	}
	p := lifecycleBuffer(t)
	SetTimeoutProvider(timeoutFixture{1700000000123, nil})
	for i := 0; i < 2; i++ {
		rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{SandboxID: "sb-timeout", Duration: 5})
		require.Equal(t, 200, rsp.Ret.RetCode)
		readUpdateDestroyEvent(t, p)
	}
	// Resume's shared refresh helper retains ordinary context and never publishes.
	refreshTimeoutMeta(context.Background(), "sb-timeout", 5)
	require.Empty(t, p.Events())
}

// Existing business seams are controlled; all producer entrypoints, capture,
// background encoding and (in Runtime tests) delivery are real.
type destroyFixture struct {
	patches                   *gomonkey.Patches
	spec                      *types.CreateCubeSandboxReq
	node                      bool
	rpcErr, specErr, proxyErr error
	enqueueErr                error
	nilReply                  bool
	pending                   *task.Task
	nodeCalls, specReads      int
	order                     []string
}

type capturedDestroyQueue struct {
	queueworker.QueueWorker
	fixture *destroyFixture
}

func (q *capturedDestroyQueue) Push(value interface{}) error {
	if q.fixture.enqueueErr != nil {
		return q.fixture.enqueueErr
	}
	q.fixture.pending = value.(*task.Task)
	return nil
}

func setupDestroyFixture(t *testing.T) *destroyFixture {
	f := &destroyFixture{patches: gomonkey.NewPatches(), spec: lifecycleSpec(), node: true}
	p := f.patches
	t.Cleanup(p.Reset)
	cfg := &config.Config{Common: &config.CommonConf{AsyncTaskQueueSize: 4, AsyncTaskWorkerNum: 4, CollectMetricInterval: time.Millisecond}, CubeletConf: &config.CubeletConf{MaxRetries: 1, LoopMaxRetries: 1}}
	p.ApplyFunc(config.GetConfig, func() *config.Config { return cfg })
	p.ApplyFunc(ResolveSandboxID, func(_ context.Context, id string) (string, error) { return id, nil })
	p.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache {
		return &localcache.SandboxCache{SandboxID: "sb-destroy", HostIP: "127.0.0.1"}
	})
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return &node.Node{IP: "127.0.0.1"}, f.node })
	p.ApplyFunc(cubelet.GetCubeletAddr, func(string) string { return "127.0.0.1:1" })
	p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		f.specReads++
		f.order = append(f.order, "spec-get")
		return f.spec, nil
	})
	p.ApplyFunc(sandboxspec.Put, func(context.Context, string, *types.CreateCubeSandboxReq, sandboxspec.PutOptions) error {
		f.order = append(f.order, "spec-put")
		return f.specErr
	})
	p.ApplyFunc(pausesnap.TryDeletePaused, func(context.Context, string, string, string) (bool, error) { return false, nil })
	p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { f.order = append(f.order, "proxy-delete"); return f.proxyErr })
	p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		f.nodeCalls++
		f.order = append(f.order, "node-destroy")
		if f.rpcErr != nil {
			return nil, f.rpcErr
		}
		if f.nilReply {
			return nil, nil
		}
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	p.ApplyFunc(cubelet.Update, func(context.Context, string, *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
		f.nodeCalls++
		f.order = append(f.order, "node-network")
		if f.rpcErr != nil {
			return nil, f.rpcErr
		}
		if f.nilReply {
			return nil, nil
		}
		return &cubebox.UpdateCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	// Capture admission synchronously for deterministic boundary tests, behind
	// the real InitTask interface dispatch. The App test exercises the unwrapped
	// queue, real worker handler and its retry path separately.
	worker := queueworker.NewQueueWorker(&queueworker.Options{QueueSize: 4, WorkerNum: 4}, func(interface{}) error { return nil })
	p.ApplyFunc(queueworker.NewQueueWorker, func(options *queueworker.Options, _ queueworker.WorkHandler) queueworker.QueueWorker {
		require.Equal(t, 4, options.QueueSize)
		require.Equal(t, 4, options.WorkerNum)
		return &capturedDestroyQueue{QueueWorker: worker, fixture: f}
	})
	ctx, cancel := context.WithCancel(context.Background())
	task.InitTask(ctx, cfg)
	t.Cleanup(func() { task.Stop(context.Background()); cancel() })
	return f
}
func networkFixture() *types.UpdateNetworkRequest {
	no := false
	return &types.UpdateNetworkRequest{SandboxID: "sb-destroy", InstanceType: "cubebox", CubeNetworkConfig: &types.CubeNetworkConfig{AllowInternetAccess: &no, AllowOut: []string{"8.8.8.8", "10.1.2.3/8", "*.Example.COM"}, DenyOut: []string{"1.2.3.4"}}}
}
func TestNetworkBoundary(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	for _, scenario := range []string{"success", "spec-write-fail", "spec-missing", "rpc-fail", "nil", "oversize", "same", "l7-empty", "mixed", "l7-only"} {
		t.Run(scenario, func(t *testing.T) {
			f := setupDestroyFixture(t)
			p := lifecycleBuffer(t)
			req := networkFixture()
			switch scenario {
			case "spec-write-fail":
				f.specErr = errors.New("spec")
			case "spec-missing":
				f.spec = nil
			case "rpc-fail":
				f.rpcErr = errors.New("rpc")
			case "nil":
				f.nilReply = true
			case "oversize":
				req.CubeNetworkConfig.AllowOut = []string{strings.Repeat("a", 17000)}
			case "l7-empty":
				req.CubeNetworkConfig = &types.CubeNetworkConfig{}
			}
			if scenario == "mixed" || scenario == "l7-only" {
				if scenario == "l7-only" {
					req.CubeNetworkConfig = &types.CubeNetworkConfig{}
				}
				host := "private.example"
				req.CubeNetworkConfig.Rules = []*types.EgressRule{{Match: &types.EgressRuleMatch{Host: &host}, Action: &types.EgressRuleAction{Allow: true, Inject: []*types.EgressRuleInject{{Header: "Authorization", Secret: "never-export"}}}}}
			}
			before, _ := json.Marshal(req)
			rsp := UpdateNetwork(context.Background(), req)
			after, _ := json.Marshal(req)
			require.Equal(t, string(before), string(after))
			if scenario == "rpc-fail" || scenario == "nil" || scenario == "oversize" {
				require.Empty(t, p.Events())
				return
			}
			require.Equal(t, 200, rsp.Ret.RetCode)
			body := readUpdateDestroyEvent(t, p)
			data := body["event_data"].(map[string]any)
			n := data["network_egress"].(map[string]any)
			require.Len(t, n, 3)
			wire, _ := json.Marshal(body)
			require.NotContains(t, string(wire), "never-export")
			require.NotContains(t, string(wire), "private.example")
			if scenario != "l7-empty" && scenario != "l7-only" {
				require.Equal(t, []any{"0.0.0.0/0"}, n["denied_cidrs"])
				require.Equal(t, []any{"8.8.8.8/32", "10.0.0.0/8"}, n["allowed_cidrs"])
				require.Equal(t, []any{"*.example.com"}, n["allowed_domains"])
			}
			require.Equal(t, 1, f.specReads)
			if scenario == "same" {
				require.Equal(t, 200, UpdateNetwork(context.Background(), req).Ret.RetCode)
				readUpdateDestroyEvent(t, p)
			}
			require.Empty(t, p.Events())
		})
	}
}
func TestDestroyBoundary(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	for _, async := range []bool{false, true} {
		for _, scenario := range []string{"success", "rpc-fail", "nil", "node-missing", "cleanup-fail", "internal", "unknown", "timeout", "oversize"} {
			t.Run(scenario+map[bool]string{true: "/async", false: "/sync"}[async], func(t *testing.T) {
				f := setupDestroyFixture(t)
				p := lifecycleBuffer(t)
				task.SetWebhookPublisher(p)
				defer task.SetWebhookPublisher(nil)
				ctx := context.Background()
				reason := "request"
				switch scenario {
				case "rpc-fail":
					f.rpcErr = errors.New("rpc")
				case "nil":
					f.nilReply = true
				case "node-missing":
					f.node = false
				case "cleanup-fail":
					f.proxyErr = errors.New("proxy")
				case "internal":
					ctx = webhook.WithOrigin(ctx, webhook.OriginCreateFailover)
				case "unknown":
					reason = ""
				case "timeout":
					reason = "timeout"
				case "oversize":
					reason = strings.Repeat("r", 5000)
				}
				rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: !async, KillReason: reason})
				var err error
				if async {
					require.Equal(t, 200, rsp.Ret.RetCode)
					require.Empty(t, p.Events())
					require.NotNil(t, f.pending)
					err = (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending)
					if scenario == "cleanup-fail" {
						require.Error(t, err)
						require.Empty(t, p.Events())
						f.proxyErr = nil
						err = (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending)
						require.NoError(t, err)
						require.Equal(t, 2, f.nodeCalls, "cleanup retry must repeat the node operation")
					}
				}
				want := scenario == "success" || scenario == "unknown" || scenario == "timeout" || scenario == "nil" || scenario == "node-missing" || (async && scenario == "cleanup-fail")
				if want {
					body := readUpdateDestroyEvent(t, p)
					data := body["event_data"].(map[string]any)
					if reason == "" {
						reason = "unknown"
					}
					require.Equal(t, reason, data["kill_reason"])
					require.NotContains(t, data, "sandbox_metadata")
					require.NotContains(t, data["execution"].(map[string]any), "vcpu_count")
					require.NotContains(t, data["execution"].(map[string]any), "memory_mb")
					require.Empty(t, body["sandbox_template_id"])
				}
				require.Empty(t, p.Events())
				if async && want {
					require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
					readUpdateDestroyEvent(t, p)
					require.Empty(t, p.Events(), "each successful business operation may create a fresh event")
				}
				require.Equal(t, 1, f.specReads)
			})
		}
	}
}
func TestUpdateDestroyRuntimeHTTP(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	r, bodies := webhooktest.Runtime(t, webhook.TypeUpdated, webhook.TypeKilled)
	f := setupDestroyFixture(t)
	SetWebhookPublisher(r.Publisher())
	task.SetWebhookPublisher(r.Publisher())
	defer SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	old := getTimeoutProvider()
	defer SetTimeoutProvider(old)
	SetTimeoutProvider(timeoutFixture{1700000000123, nil})
	require.Equal(t, 200, SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-destroy", Timeout: 5}).Ret.RetCode)
	require.Equal(t, 200, UpdateNetwork(context.Background(), networkFixture()).Ret.RetCode)
	for _, sync := range []bool{true, false} {
		require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: sync, KillReason: "request"}).Ret.RetCode)
		if !sync {
			require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
		}
	}
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		select {
		case body := <-bodies:
			var event map[string]any
			require.NoError(t, json.Unmarshal(body, &event))
			counts[event["type"].(string)]++
		case <-time.After(10 * time.Second):
			t.Fatal("delivery timeout")
		}
	}
	require.Equal(t, map[string]int{webhook.TypeUpdated: 2, webhook.TypeKilled: 2}, counts)

}
func TestUpdateDestroyNotificationFailureIsolation(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		f := setupDestroyFixture(t)
		SetWebhookPublisher(p)
		task.SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		defer task.SetWebhookPublisher(nil)
		old := getTimeoutProvider()
		defer SetTimeoutProvider(old)
		SetTimeoutProvider(timeoutFixture{1700000000123, nil})
		require.Equal(t, 200, SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-destroy", Timeout: 5}).Ret.RetCode)
		require.Equal(t, 200, UpdateNetwork(context.Background(), networkFixture()).Ret.RetCode)
		for _, sync := range []bool{true, false} {
			require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: sync}).Ret.RetCode)
			if !sync {
				require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
			}
		}
		require.Equal(t, []string{"node-network", "spec-get", "spec-put", "spec-get", "node-destroy", "proxy-delete", "spec-get", "node-destroy", "proxy-delete"}, f.order)
		require.Equal(t, 3, f.nodeCalls)
		require.Equal(t, 3, f.specReads)
	})
}

func TestDestroyCompletionBarrier(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			f := setupDestroyFixture(t)
			p := lifecycleBuffer(t)
			task.SetWebhookPublisher(p)
			defer task.SetWebhookPublisher(nil)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
				close(entered)
				<-release
				return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
			})
			req := &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: !async}
			if async {
				require.Equal(t, 200, DestroySandbox(context.Background(), req).Ret.RetCode)
			}
			go func() {
				defer close(done)
				if async {
					_ = (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending)
				} else {
					DestroySandbox(context.Background(), req)
				}
			}()
			<-entered
			require.Empty(t, p.Events())
			close(release)
			<-done
			readUpdateDestroyEvent(t, p)
			require.Empty(t, p.Events())
		})
	}
}

func TestProducerFailureBoundaries(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	for _, scenario := range []string{"lock", "enqueue", "node-code", "paused-unproven", "paused-error", "paused-binding-failed", "paused-complete", "hook-warning", "pause-origin", "resume-origin"} {
		t.Run(scenario, func(t *testing.T) {
			f := setupDestroyFixture(t)
			p := lifecycleBuffer(t)
			task.SetWebhookPublisher(p)
			defer task.SetWebhookPublisher(nil)
			ctx := context.Background()
			switch scenario {
			case "lock":
				f.patches.ApplyFunc(sandboxlock.WithLock, func(context.Context, string, sandboxlock.Options, func(context.Context) error) error {
					return sandboxlock.ErrLockNotAcquired
				})
			case "enqueue":
				f.enqueueErr = errors.New("queue")
			case "node-code":
				f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
					return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid}}, nil
				})
				f.patches.ApplyFunc(cubelet.Update, func(context.Context, string, *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
					return &cubebox.UpdateCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid}}, nil
				})
			case "paused-unproven", "paused-error", "paused-binding-failed", "paused-complete":
				f.patches.ApplyFunc(pausesnap.TryDeletePaused, func(_ context.Context, _, _, _ string) (bool, error) {
					if scenario == "paused-error" {
						return true, errors.New("cleanup")
					}
					return true, nil
				})
			case "hook-warning":
				RegisterAfterDestroySandboxSuccessHook(func(context.Context, string) error { return errors.New("warn-only") })
				defer ResetAfterDestroySandboxSuccessHooks()
			case "pause-origin":
				ctx = webhook.WithOrigin(ctx, webhook.OriginPauseTeardown)
			case "resume-origin":
				ctx = webhook.WithOrigin(ctx, webhook.OriginResumeTimeoutRefresh)
			}
			if scenario == "lock" || scenario == "node-code" {
				UpdateNetwork(ctx, networkFixture())
				require.Empty(t, p.Events())
			}
			rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: scenario != "enqueue"})
			if scenario == "paused-binding-failed" {
				require.Equal(t, 200, rsp.Ret.RetCode, "binding cleanup remains warn-only")
			}
			if scenario == "paused-complete" || scenario == "paused-unproven" || scenario == "paused-binding-failed" || scenario == "hook-warning" {
				require.Equal(t, 200, rsp.Ret.RetCode)
				readUpdateDestroyEvent(t, p)
			}
			require.Empty(t, p.Events())
		})
	}
}

func TestAsyncPausedWarnOnlyBindingFailurePublishesKilled(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	f := setupDestroyFixture(t)
	p := lifecycleBuffer(t)
	task.SetWebhookPublisher(p)
	defer task.SetWebhookPublisher(nil)
	f.patches.ApplyFunc(pausesnap.TryDeletePaused, func(_ context.Context, _, _, _ string) (bool, error) {
		return true, nil
	})
	require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox"}).Ret.RetCode)
	h := &task.DestroySandboxTaskHandler{}
	require.NoError(t, h.HandleTask(f.pending.Ctx, f.pending))
	readUpdateDestroyEvent(t, p)
	require.Empty(t, p.Events())
}
func TestTaskRetryAfterRPCFailure(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	f := setupDestroyFixture(t)
	p := lifecycleBuffer(t)
	task.SetWebhookPublisher(p)
	defer task.SetWebhookPublisher(nil)
	require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", KillReason: "evicted"}).Ret.RetCode)
	h := &task.DestroySandboxTaskHandler{}
	f.rpcErr = errors.New("uncertain")
	require.Error(t, h.HandleTask(f.pending.Ctx, f.pending))
	require.Empty(t, p.Events())
	f.rpcErr = nil
	require.NoError(t, h.HandleTask(f.pending.Ctx, f.pending))
	require.Equal(t, "evicted", readUpdateDestroyEvent(t, p)["event_data"].(map[string]any)["kill_reason"])
	require.Equal(t, 2, f.nodeCalls)
	require.Empty(t, p.Events())
	// A failed separate operation must not publish success.
	f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid, RetMsg: "not found"}}, nil
	})
	DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-destroy", InstanceType: "cubebox", Sync: true})
	require.Empty(t, p.Events())
}

func TestTimeoutNotificationUsesNormalizedIDWithoutChangingResponse(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	f := setupDestroyFixture(t)
	f.patches.ApplyFunc(ResolveSandboxID, func(context.Context, string) (string, error) { return "canonical-sandbox", nil })
	p := lifecycleBuffer(t)
	old := getTimeoutProvider()
	defer SetTimeoutProvider(old)
	SetTimeoutProvider(timeoutFixture{1700000000123, nil})
	rsp := SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "canonical", Timeout: 5})
	require.Equal(t, 200, rsp.Ret.RetCode)
	require.Equal(t, "canonical", rsp.SandboxID, "retain the existing response construction")
	require.Equal(t, "canonical-sandbox", readUpdateDestroyEvent(t, p)["sandbox_id"])
	require.Zero(t, f.nodeCalls)
	require.Zero(t, f.specReads)
}

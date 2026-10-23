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

type p5Timeout struct {
	end     int64
	applied bool
	err     error
}

func (p p5Timeout) RefreshTimeout(context.Context, string, int) (int64, bool, error) {
	return p.end, p.applied, p.err
}
func (p p5Timeout) LookupEndAt(context.Context, string) (int64, error) { return p.end, p.err }
func p5Read(t *testing.T, p *webhook.BufferedPublisher) map[string]any {
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
func TestP5TimeoutAuthority(t *testing.T) {
	localcache.SetSandboxCache("sb-p5", &localcache.SandboxCache{SandboxID: "sb-p5", HostIP: "127.0.0.1"})
	defer localcache.DeleteSandboxCache("sb-p5")
	old := getTimeoutProvider()
	defer SetTimeoutProvider(old)
	for _, tc := range []struct {
		name   string
		p      TimeoutProvider
		value  int32
		origin webhook.Origin
		want   bool
	}{
		{"finite", p5Timeout{1700000000123, true, nil}, 5, webhook.OriginLifecycle, true},
		{"zero", p5Timeout{1700000000123, true, nil}, 0, webhook.OriginLifecycle, true},
		{"infinite", p5Timeout{0, true, nil}, -1, webhook.OriginLifecycle, false},
		{"not-applied", p5Timeout{1700000000123, false, nil}, 5, webhook.OriginLifecycle, false},
		{"failed", p5Timeout{1700000000123, false, errors.New("write")}, 5, webhook.OriginLifecycle, false},
		{"missing", nil, 5, webhook.OriginLifecycle, false},
		{"zero-result", p5Timeout{0, true, nil}, 5, webhook.OriginLifecycle, false},
		{"resume", p5Timeout{1700000000123, true, nil}, 5, webhook.OriginResumeTimeoutRefresh, false},
		{"invalid", p5Timeout{1700000000123, true, nil}, -2, webhook.OriginLifecycle, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := p4Buffer(t)
			SetTimeoutProvider(tc.p)
			rsp := SetTimeout(webhook.WithOrigin(context.Background(), tc.origin), &types.SetTimeoutRequest{SandboxID: "sb-p5", Timeout: tc.value})
			if tc.value >= -1 {
				require.Equal(t, 200, rsp.Ret.RetCode)
			}
			if tc.want {
				body := p5Read(t, p)
				require.Equal(t, "2023-11-14T22:13:20Z", body["event_data"].(map[string]any)["set_timeout"])
			}
			require.Empty(t, p.Events())
		})
	}
	p := p4Buffer(t)
	SetTimeoutProvider(p5Timeout{1700000000123, true, nil})
	for i := 0; i < 2; i++ {
		rsp := Refresh(context.Background(), &types.RefreshSandboxRequest{SandboxID: "sb-p5", Duration: 5})
		require.Equal(t, 200, rsp.Ret.RetCode)
		p5Read(t, p)
	}
}

// Existing business seams are controlled; all producer entrypoints, capture,
// background encoding and (in Runtime tests) delivery are real.
type p5Fixture struct {
	patches                   *gomonkey.Patches
	spec                      *types.CreateCubeSandboxReq
	node                      bool
	rpcErr, specErr, proxyErr error
	nilReply                  bool
	pending                   *task.Task
	nodeCalls, specReads      int
}

func p5Setup(t *testing.T) *p5Fixture {
	f := &p5Fixture{patches: gomonkey.NewPatches(), spec: p4Spec(), node: true}
	p := f.patches
	t.Cleanup(p.Reset)
	p.ApplyFunc(config.GetConfig, func() *config.Config { return &config.Config{Common: &config.CommonConf{}} })
	p.ApplyFunc(ResolveSandboxID, func(_ context.Context, id string) (string, error) { return id, nil })
	p.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache {
		return &localcache.SandboxCache{SandboxID: "sb-p5", HostIP: "127.0.0.1"}
	})
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return &node.Node{IP: "127.0.0.1"}, f.node })
	p.ApplyFunc(cubelet.GetCubeletAddr, func(string) string { return "127.0.0.1:1" })
	p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
		return fn(ctx)
	})
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) { f.specReads++; return f.spec, nil })
	p.ApplyFunc(sandboxspec.Put, func(context.Context, string, *types.CreateCubeSandboxReq, sandboxspec.PutOptions) error {
		return f.specErr
	})
	p.ApplyFunc(pausesnap.TryDeletePausedWithEvidence, func(context.Context, string, string, string, *bool) (bool, error) { return false, nil })
	p.ApplyFunc(localcache.DeleteSandboxProxyMap, func(context.Context, string) error { return f.proxyErr })
	p.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		f.nodeCalls++
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
		if f.rpcErr != nil {
			return nil, f.rpcErr
		}
		if f.nilReply {
			return nil, nil
		}
		return &cubebox.UpdateCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	p.ApplyFunc(task.AddAsyncTask, func(job *task.Task) error { f.pending = job; return nil })
	return f
}
func p5Network() *types.UpdateNetworkRequest {
	no := false
	return &types.UpdateNetworkRequest{SandboxID: "sb-p5", InstanceType: "cubebox", CubeNetworkConfig: &types.CubeNetworkConfig{AllowInternetAccess: &no, AllowOut: []string{"8.8.8.8", "10.1.2.3/8", "*.Example.COM"}, DenyOut: []string{"1.2.3.4"}}}
}
func TestP5NetworkBoundary(t *testing.T) {
	for _, scenario := range []string{"success", "spec-write-fail", "spec-missing", "rpc-fail", "nil", "oversize", "same", "l7-empty", "mixed", "l7-only"} {
		t.Run(scenario, func(t *testing.T) {
			f := p5Setup(t)
			p := p4Buffer(t)
			req := p5Network()
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
			body := p5Read(t, p)
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
				p5Read(t, p)
			}
			require.Empty(t, p.Events())
		})
	}
}
func TestP5DestroyBoundary(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, scenario := range []string{"success", "rpc-fail", "nil", "node-missing", "cleanup-fail", "internal", "unknown", "timeout", "oversize"} {
			t.Run(scenario+map[bool]string{true: "/async", false: "/sync"}[async], func(t *testing.T) {
				f := p5Setup(t)
				p := p4Buffer(t)
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
				rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: !async, KillReason: reason})
				var err error
				if async {
					require.Equal(t, 200, rsp.Ret.RetCode)
					require.Empty(t, p.Events())
					require.NotNil(t, f.pending)
					f.spec.WebhookMetadata.Values["user_token"] = "mutated"
					err = (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending)
					if scenario == "cleanup-fail" {
						require.Error(t, err)
						require.Empty(t, p.Events())
						f.proxyErr = nil
						f.node = false
						err = (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending)
						require.NoError(t, err)
						require.Equal(t, 1, f.nodeCalls)
					}
				}
				want := scenario == "success" || scenario == "unknown" || scenario == "timeout" || (async && scenario == "cleanup-fail")
				if want {
					body := p5Read(t, p)
					data := body["event_data"].(map[string]any)
					if reason == "" {
						reason = "unknown"
					}
					require.Equal(t, reason, data["kill_reason"])
					require.Equal(t, "kept", data["sandbox_metadata"].(map[string]any)["user_token"])
					require.Equal(t, float64(1), data["execution"].(map[string]any)["vcpu_count"])
				}
				require.Empty(t, p.Events())
				if async && want {
					require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
					require.Empty(t, p.Events())
				}
				require.Equal(t, 1, f.specReads)
			})
		}
	}
}
func TestP5BusinessRedisHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, 13, webhook.TypeUpdated, webhook.TypeKilled)
	f := p5Setup(t)
	SetWebhookPublisher(r.Publisher())
	task.SetWebhookPublisher(r.Publisher())
	defer SetWebhookPublisher(nil)
	defer task.SetWebhookPublisher(nil)
	old := getTimeoutProvider()
	defer SetTimeoutProvider(old)
	SetTimeoutProvider(p5Timeout{1700000000123, true, nil})
	require.Equal(t, 200, SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-p5", Timeout: 5}).Ret.RetCode)
	require.Equal(t, 200, UpdateNetwork(context.Background(), p5Network()).Ret.RetCode)
	for _, sync := range []bool{true, false} {
		require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: sync, KillReason: "request"}).Ret.RetCode)
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
func TestP5BusinessFailureIsolation(t *testing.T) {
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		f := p5Setup(t)
		SetWebhookPublisher(p)
		task.SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		defer task.SetWebhookPublisher(nil)
		old := getTimeoutProvider()
		defer SetTimeoutProvider(old)
		SetTimeoutProvider(p5Timeout{1700000000123, true, nil})
		require.Equal(t, 200, SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb-p5", Timeout: 5}).Ret.RetCode)
		require.Equal(t, 200, UpdateNetwork(context.Background(), p5Network()).Ret.RetCode)
		for _, sync := range []bool{true, false} {
			require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: sync}).Ret.RetCode)
			if !sync {
				require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
			}
		}
	})
}

func TestP5DestroyCompletionBarrier(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "async"}[async], func(t *testing.T) {
			f := p5Setup(t)
			p := p4Buffer(t)
			task.SetWebhookPublisher(p)
			defer task.SetWebhookPublisher(nil)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
				close(entered)
				<-release
				return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
			})
			req := &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: !async}
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
			p5Read(t, p)
			require.Empty(t, p.Events())
		})
	}
}

func TestP5ProducerFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"lock", "enqueue", "node-code", "paused-unproven", "paused-error", "paused-complete", "hook-warning", "pause-origin", "resume-origin"} {
		t.Run(scenario, func(t *testing.T) {
			f := p5Setup(t)
			p := p4Buffer(t)
			task.SetWebhookPublisher(p)
			defer task.SetWebhookPublisher(nil)
			ctx := context.Background()
			switch scenario {
			case "lock":
				f.patches.ApplyFunc(sandboxlock.WithLock, func(context.Context, string, sandboxlock.Options, func(context.Context) error) error {
					return sandboxlock.ErrLockNotAcquired
				})
			case "enqueue":
				f.patches.ApplyFunc(task.AddAsyncTask, func(*task.Task) error { return errors.New("queue") })
			case "node-code":
				f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
					return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid}}, nil
				})
				f.patches.ApplyFunc(cubelet.Update, func(context.Context, string, *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
					return &cubebox.UpdateCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid}}, nil
				})
			case "paused-unproven", "paused-error", "paused-complete":
				f.patches.ApplyFunc(pausesnap.TryDeletePausedWithEvidence, func(_ context.Context, _, _, _ string, evidence *bool) (bool, error) {
					if scenario == "paused-error" {
						*evidence = true
						return true, errors.New("cleanup")
					}
					*evidence = scenario == "paused-complete"
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
				UpdateNetwork(ctx, p5Network())
				require.Empty(t, p.Events())
			}
			rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: scenario != "enqueue"})
			if scenario == "paused-complete" || scenario == "hook-warning" {
				require.Equal(t, 200, rsp.Ret.RetCode)
				p5Read(t, p)
			}
			require.Empty(t, p.Events())
		})
	}
}
func TestP5TaskRetryAfterRPCFailure(t *testing.T) {
	f := p5Setup(t)
	p := p4Buffer(t)
	task.SetWebhookPublisher(p)
	defer task.SetWebhookPublisher(nil)
	require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", KillReason: "evicted"}).Ret.RetCode)
	h := &task.DestroySandboxTaskHandler{}
	f.rpcErr = errors.New("uncertain")
	require.Error(t, h.HandleTask(f.pending.Ctx, f.pending))
	require.Empty(t, p.Events())
	f.rpcErr = nil
	require.NoError(t, h.HandleTask(f.pending.Ctx, f.pending))
	require.Equal(t, "evicted", p5Read(t, p)["event_data"].(map[string]any)["kill_reason"])
	require.Equal(t, 2, f.nodeCalls)
	require.Empty(t, p.Events())
	// A separate request has no right to reuse the previous Task's evidence.
	f.patches.ApplyFunc(cubelet.Destroy, func(context.Context, string, *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
		return &cubebox.DestroyCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_TaskStateInvalid, RetMsg: "not found"}}, nil
	})
	DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb-p5", InstanceType: "cubebox", Sync: true})
	require.Empty(t, p.Events())
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubeproxy"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"testing"
	"time"
)

func p4Spec() *types.CreateCubeSandboxReq {
	return &types.CreateCubeSandboxReq{InstanceType: "cubebox", Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "original-template"}, WebhookMetadata: webhook.UserMetadata(map[string]string{"user_token": "kept"}), Containers: []*types.Container{{Resources: &types.Resource{Cpu: "1", Mem: "1Gi"}}}}
}
func p4Buffer(t *testing.T) *webhook.BufferedPublisher {
	p, e := webhook.NewBufferedPublisher(32)
	require.NoError(t, e)
	SetWebhookPublisher(p)
	t.Cleanup(func() { SetWebhookPublisher(nil) })
	return p
}
func p4Read(t *testing.T, p *webhook.BufferedPublisher, kind string) {
	t.Helper()
	select {
	case c := <-p.Events():
		d, e := webhook.MapToProtocol(c)
		require.NoError(t, e)
		ev, e := webhook.Encode(c, d)
		require.NoError(t, e)
		require.Equal(t, kind, ev.Type())
		var body map[string]any
		require.NoError(t, json.Unmarshal(ev.Body(), &body))
		require.Equal(t, "original-template", body["sandbox_template_id"])
		data := body["event_data"].(map[string]any)
		require.Equal(t, "kept", data["sandbox_metadata"].(map[string]any)["user_token"])
	default:
		t.Fatal("missing event")
	}
	require.Empty(t, p.Events())
}
func p4Create(t *testing.T, ctx context.Context, proxyErr error) *createSandboxContext {
	old := setSandboxProxyMapFn
	t.Cleanup(func() { setSandboxProxyMapFn = old })
	setSandboxProxyMapFn = func(context.Context, *proxytypes.SandboxProxyMap) error { return proxyErr }
	return &createSandboxContext{ctx: withCreateOriginRequest(ctx, p4Spec()), selectHost: &node.Node{IP: "127.0.0.1", InsID: "node"}, masterRsp: &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: -1}, ExtInfo: map[string]string{}}, cubeletReq: &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"}, cubeletRsp: &cubebox.RunCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SandboxID: "sb", SandboxIP: "10.0.0.2"}}
}
func TestP4CreateFinalBoundary(t *testing.T) {
	for _, scenario := range []string{"success", "snapshot", "forged", "proxy-error", "cancel-wins", "spec-warning", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			p := p4Buffer(t)
			ctx := context.Background()
			var err error
			if scenario == "snapshot" {
				ctx = webhook.WithResolvedStart(ctx, "original-template", true)
			}
			if scenario == "proxy-error" {
				err = errors.New("proxy")
			}
			c := p4Create(t, ctx, err)
			if scenario == "oversize" {
				createOriginRequestFromContext(c.ctx).WebhookMetadata = &types.WebhookMetadata{TooLarge: true}
			}
			if scenario == "forged" {
				createOriginRequestFromContext(c.ctx).Annotations[constants.CubeAnnotationRuntimeSnapshotID] = "snap-forged"
			}
			if scenario == "cancel-wins" {
				c.setMasterRsp(int(errorcode.ErrorCode_ClientCancel), "cancel")
			}
			patch := gomonkey.ApplyFunc(runAfterCreateSandboxSuccessHook, func(context.Context, string, string, string, *types.CreateCubeSandboxReq) error {
				if scenario == "spec-warning" {
					return errors.New("spec")
				}
				return nil
			})
			defer patch.Reset()
			c.dealSuccResult()
			if scenario == "proxy-error" || scenario == "cancel-wins" {
				require.Empty(t, p.Events())
				require.NotEqual(t, int(errorcode.ErrorCode_Success), c.masterRsp.Ret.RetCode)
				return
			}
			require.Equal(t, int(errorcode.ErrorCode_Success), c.masterRsp.Ret.RetCode)
			if scenario == "oversize" {
				require.Empty(t, p.Events())
				return
			}
			kind := webhook.TypeCreated
			if scenario == "snapshot" {
				kind = webhook.TypeResumed
			}
			p4Read(t, p, kind)
			c.dealSuccResult()
			require.Empty(t, p.Events())
		})
	}
}
func p4PausePatches(t *testing.T, failure, backend string) *gomonkey.Patches {
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return nil, false })
	p.ApplyFunc(clearStalePauseBindingIfRunning, func(context.Context, string, string, string) {})
	var stored *types.CreateCubeSandboxReq
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		s := p4Spec()
		s.Backend = backend
		stored = s
		return s, nil
	})
	if failure != "real-store" {
		p.ApplyFunc(pausesnap.Begin, func(context.Context, string, string, string, string, string) (string, error) { return "snap", nil })
		p.ApplyFunc(pausesnap.MarkFailed, func(context.Context, string, string, string) {})
		p.ApplyFunc(pausesnap.Complete, func(context.Context, string, string, string, string, string, []string, string) error {
			if failure == "complete" {
				return errors.New("complete")
			}
			return nil
		})
	}
	p.ApplyFunc(cubelet.UpdateWithTimeout, func(ctx context.Context, _ string, _ *cubebox.UpdateCubeSandboxRequest, _ time.Duration) (*cubebox.UpdateCubeSandboxResponse, error) {
		stored.WebhookMetadata.Values["user_token"] = "after-cleanup"
		stored.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = "after-cleanup"
		require.Equal(t, webhook.OriginPauseTeardown, webhook.OriginFromContext(ctx))
		if failure == "timeout" {
			return nil, context.DeadlineExceeded
		}
		if failure == "nil" {
			return nil, nil
		}
		if failure == "nil-ret" {
			return &cubebox.UpdateCubeSandboxResponse{}, nil
		}
		code := ec.ErrorCode_Success
		if failure == "node" {
			code = ec.ErrorCode_Unknown
		}
		rsp := &cubebox.UpdateCubeSandboxResponse{Ret: &ec.Ret{RetCode: code}}
		if failure == "real-store" {
			rsp.RemoteUuids = "export-pending"
		}
		return rsp, nil
	})
	p.ApplyFunc(runAfterUpdateSandboxSuccessHook, func(context.Context, string, string, string, string) {})
	return p
}
func TestP4PauseLocalBoundary(t *testing.T) {
	for _, backend := range []string{"xfs", "s3"} {
		for _, failure := range []string{"", "node", "timeout", "nil", "nil-ret", "complete"} {
			t.Run(backend+failure, func(t *testing.T) {
				p := p4Buffer(t)
				p4PausePatches(t, failure, backend)
				rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
				if failure != "" {
					require.NotEqual(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
					require.Empty(t, p.Events())
				} else {
					require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
					p4Read(t, p, webhook.TypePaused)
				}
			})
		}
	}
}
func p4ResumePatches(t *testing.T, failure string) {
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{SnapshotID: "snap", Status: "READY", NodeIP: "127.0.0.1", Backend: "xfs"}, nil
	})
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) { return p4Spec(), nil })
	p.ApplyFunc(cubelet.Create, func(ctx context.Context, _ string, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
		require.NoError(t, ctx.Err())
		if failure == "node" {
			return nil, errors.New("node")
		}
		id := req.Annotations[constants.CubeAnnotationDesiredSandboxID]
		if failure == "identity" {
			id = "wrong"
		}
		return &cubebox.RunCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SandboxID: id, SandboxIP: "10.0.0.1"}, nil
	})
	p.ApplyFunc(refreshProxyMapAfterResume, func(context.Context, string, string, *cubebox.RunCubeSandboxResponse) error {
		if failure == "proxy" {
			return errors.New("proxy")
		}
		return nil
	})
	p.ApplyFunc(cubeproxy.InvalidateBackendCache, func(context.Context, string, string) error {
		if failure == "purge" {
			return errors.New("purge")
		}
		return nil
	})
	p.ApplyFunc(pausesnap.CleanupPauseSnapshot, func(context.Context, string, string, string, string) error { return errors.New("warn-only cleanup") })
	p.ApplyFunc(pausesnap.Delete, func(context.Context, string) error { return errors.New("warn-only binding") })
	p.ApplyFunc(runAfterUpdateSandboxSuccessHook, func(context.Context, string, string, string, string) {})
}
func TestP4ResumeFinalBoundary(t *testing.T) {
	for _, failure := range []string{"", "node", "identity", "proxy", "purge"} {
		t.Run(failure, func(t *testing.T) {
			p := p4Buffer(t)
			p4ResumePatches(t, failure)
			rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			if failure != "" {
				require.NotEqual(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				require.Empty(t, p.Events())
			} else {
				require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				p4Read(t, p, webhook.TypeResumed)
			}
		})
	}
}

func TestP4BusinessRedisHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, 10, webhook.TypeCreated, webhook.TypeResumed, webhook.TypePaused)
	SetWebhookPublisher(r.Publisher())
	t.Cleanup(func() { SetWebhookPublisher(nil) })
	c := p4Create(t, context.Background(), nil)
	c.dealSuccResult()
	t.Run("pause", func(t *testing.T) {
		p4PausePatches(t, "", "s3")
		rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	})
	t.Run("resume", func(t *testing.T) {
		p4ResumePatches(t, "")
		rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	})
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case b := <-bodies:
			var v map[string]any
			require.NoError(t, json.Unmarshal(b, &v))
			seen[v["type"].(string)] = true
			require.Equal(t, "original-template", v["sandbox_template_id"])
		case <-time.After(8 * time.Second):
			t.Fatal("pipeline timeout")
		}
	}
	require.Len(t, seen, 3)
}

func TestP4CreateCancellationDuringPostProcessing(t *testing.T) {
	p := p4Buffer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := p4Create(t, ctx, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	setSandboxProxyMapFn = func(context.Context, *proxytypes.SandboxProxyMap) error { close(entered); <-release; return nil }
	c.done = make(chan struct{})
	c.cancel = func() {}
	finished := make(chan struct{})
	go func() { defer close(finished); c.dealSuccResult(); close(c.done) }()
	<-entered
	cancel()
	c.Wait()
	require.Equal(t, int(errorcode.ErrorCode_ClientCancel), c.masterRsp.Ret.RetCode)
	close(release)
	<-finished
	require.Empty(t, p.Events())
}

type p4TimeoutProvider struct{ seen []webhook.Origin }

func (p *p4TimeoutProvider) RefreshTimeout(ctx context.Context, _ string, _ int) (int64, error) {
	p.seen = append(p.seen, webhook.OriginFromContext(ctx))
	return 100, nil
}
func (p *p4TimeoutProvider) LookupEndAt(context.Context, string) (int64, error) { return 0, nil }
func TestP4ResumeTimeoutOrigin(t *testing.T) {
	p := &p4TimeoutProvider{}
	old := getTimeoutProvider()
	SetTimeoutProvider(p)
	defer SetTimeoutProvider(old)
	ctx := context.Background()
	for _, value := range []int{0, 5, -1} {
		v := value
		publishUpdateTimeout(ctx, &types.UpdateRequest{Action: "resume", SandboxID: "sb", Timeout: &v})
	}
	publishUpdateTimeout(ctx, &types.UpdateRequest{Action: "resume", SandboxID: "sb"})
	require.Equal(t, []webhook.Origin{webhook.OriginResumeTimeoutRefresh, webhook.OriginResumeTimeoutRefresh}, p.seen)
	require.Equal(t, webhook.OriginLifecycle, webhook.OriginFromContext(ctx))
}
func TestP4FailoverEnqueuesOrigin(t *testing.T) {
	c := p4Create(t, context.Background(), nil)
	c.masterRsp.SandboxID = "sb"
	c.setMasterRsp(int(errorcode.ErrorCode_DBError), "fail")
	old := addCreateFailoverTaskFn
	defer func() { addCreateFailoverTaskFn = old }()
	addCreateFailoverTaskFn = func(job *task.Task) error {
		require.Equal(t, webhook.OriginCreateFailover, webhook.OriginFromContext(job.Ctx))
		require.Equal(t, task.DestroySandbox, job.TaskType)
		return nil
	}
	c.failover()
}

func TestP4BusinessFailureIsolation(t *testing.T) {
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		c := p4Create(t, context.Background(), nil)
		c.dealSuccResult()
		require.Equal(t, int(errorcode.ErrorCode_Success), c.masterRsp.Ret.RetCode)
		t.Run("pause", func(t *testing.T) {
			p4PausePatches(t, "", "s3")
			rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		})
		t.Run("resume", func(t *testing.T) {
			p4ResumePatches(t, "")
			rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		})
	})
}

func TestP4UpdateDetachedCancellationAndResumeTimeout(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		for _, timeout := range []*int{nil, types.TimeoutPtr(0), types.TimeoutPtr(5), types.TimeoutPtr(-1)} {
			t.Run(action, func(t *testing.T) {
				buffer := p4Buffer(t)
				if action == "pause" {
					p4PausePatches(t, "", "s3")
				} else {
					p4ResumePatches(t, "")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p := gomonkey.NewPatches()
				defer p.Reset()
				p.ApplyFunc(sandboxlock.WithLock, func(ctx context.Context, _ string, _ sandboxlock.Options, fn func(context.Context) error) error {
					require.NoError(t, ctx.Err())
					cancel()
					return fn(ctx)
				})
				p.ApplyFunc(localcache.ListKnownSandboxIDs, func() []string { return []string{"sb"} })
				p.ApplyFunc(localcache.GetSandboxCache, func(string) *localcache.SandboxCache { return &localcache.SandboxCache{HostIP: "127.0.0.1"} })
				provider := &p4TimeoutProvider{}
				old := getTimeoutProvider()
				SetTimeoutProvider(provider)
				defer SetTimeoutProvider(old)
				rsp := Update(ctx, &types.UpdateRequest{RequestID: "req", SandboxID: "sb", InstanceType: "cubebox", Action: action, Timeout: timeout})
				require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				kind := webhook.TypePaused
				if action == "resume" {
					kind = webhook.TypeResumed
				}
				p4Read(t, buffer, kind)
				if action == "resume" && timeout != nil && *timeout != 0 {
					require.Equal(t, []webhook.Origin{webhook.OriginResumeTimeoutRefresh}, provider.seen)
				} else {
					require.Empty(t, provider.seen)
				}
			})
		}
	}
}

func TestP4PauseDuplicateAndUploadResult(t *testing.T) {
	for _, terminal := range []string{constants.RemoteStatusReady, constants.RemoteStatusFailed} {
		t.Run(terminal, func(t *testing.T) {
			db := webhooktest.SnapshotDB(t)
			pausesnap.Init(db)
			defer pausesnap.Init(nil)
			p := p4Buffer(t)
			p4PausePatches(t, "real-store", "s3")
			req := &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}
			first := pauseSandbox(context.Background(), req, "127.0.0.1")
			require.Equal(t, int(errorcode.ErrorCode_Success), first.Ret.RetCode)
			var row models.PauseSnapshotRecord
			require.NoError(t, db.Where("sandbox_id = ?", "sb").First(&row).Error)
			require.Equal(t, constants.RemoteStatusInProgress, row.RemoteStatus)
			require.Len(t, p.Events(), 1)
			again := pauseSandbox(context.Background(), req, "127.0.0.1")
			require.NotEqual(t, int(errorcode.ErrorCode_Success), again.Ret.RetCode)
			require.Contains(t, again.Ret.RetMsg, "already has pause snapshot")
			require.Len(t, p.Events(), 1)
			webhooktest.ReconcileUpload(t, db, constants.PauseSnapshotTableName, row.SnapshotID, terminal, p)
			p4Read(t, p, webhook.TypePaused)
		})
	}
}

func TestP4PauseTimeoutHealingDoesNotPublish(t *testing.T) {
	db := webhooktest.SnapshotDB(t)
	pausesnap.Init(db)
	defer pausesnap.Init(nil)
	p := p4Buffer(t)
	row := &models.PauseSnapshotRecord{SnapshotID: "snap-timeout", SandboxID: "sb", NodeID: "node", NodeIP: "127.0.0.1", InstanceType: "cubebox", Backend: "s3", Status: "FAILED", LastError: "rpc error: code = DeadlineExceeded desc = context deadline exceeded"}
	require.NoError(t, db.Create(row).Error)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(cubeletReportsPaused, func(context.Context, string, string) bool { return true })
	rec, err := pausesnap.GetBySandbox(context.Background(), "sb")
	require.NoError(t, err)
	healed, err := recoverTimedOutPauseForResume(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, rec, "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, "READY", healed.Status)
	require.Empty(t, p.Events(), "healing a timed-out pause is not a new successful pause")
}

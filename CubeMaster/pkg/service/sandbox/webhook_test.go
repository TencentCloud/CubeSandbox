// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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

func lifecycleSpec() *types.CreateCubeSandboxReq {
	return &types.CreateCubeSandboxReq{InstanceType: "cubebox", Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "original-template"}, Containers: []*types.Container{{Resources: &types.Resource{Cpu: "1", Mem: "1Gi"}}}}
}
func lifecycleBuffer(t *testing.T) *webhook.BufferedPublisher {
	p, e := webhook.NewBufferedPublisher(32)
	require.NoError(t, e)
	SetWebhookPublisher(p)
	t.Cleanup(func() { SetWebhookPublisher(nil) })
	return p
}
func readLifecycleEvent(t *testing.T, p *webhook.BufferedPublisher, kind string) {
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
		require.NotContains(t, data, "sandbox_metadata")
	default:
		t.Fatal("missing event")
	}
	require.Empty(t, p.Events())
}
func createLifecycleFixture(t *testing.T, ctx context.Context, proxyErr error) *createSandboxContext {
	old := setSandboxProxyMapFn
	t.Cleanup(func() { setSandboxProxyMapFn = old })
	setSandboxProxyMapFn = func(context.Context, *proxytypes.SandboxProxyMap) error { return proxyErr }
	return &createSandboxContext{ctx: withCreateOriginRequest(ctx, lifecycleSpec()), selectHost: &node.Node{IP: "127.0.0.1", InsID: "node"}, masterRsp: &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: -1}, ExtInfo: map[string]string{}}, cubeletReq: &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"}, cubeletRsp: &cubebox.RunCubeSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SandboxID: "sb", SandboxIP: "10.0.0.2"}}
}
func TestInternalCreateResultDoesNotPublish(t *testing.T) {
	for _, scenario := range []string{"success", "proxy-error", "cancel-wins", "spec-warning"} {
		t.Run(scenario, func(t *testing.T) {
			p := lifecycleBuffer(t)
			ctx := context.Background()
			var err error
			if scenario == "proxy-error" {
				err = errors.New("proxy")
			}
			c := createLifecycleFixture(t, ctx, err)
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
			require.Empty(t, p.Events(), "internal create success never publishes; public HTTP owns this hook")
			c.dealSuccResult()
			require.Empty(t, p.Events())
		})
	}
}
func patchPauseDependencies(t *testing.T, failure, backend string) *gomonkey.Patches {
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(localcache.GetNodesByIp, func(string) (*node.Node, bool) { return nil, false })
	p.ApplyFunc(clearStalePauseBindingIfRunning, func(context.Context, string, string, string) {})
	var stored *types.CreateCubeSandboxReq
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		s := lifecycleSpec()
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
		stored.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = "after-cleanup"
		require.Equal(t, webhook.OriginLifecycle, webhook.OriginFromContext(ctx))
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
func TestPauseLocalBoundary(t *testing.T) {
	for _, backend := range []string{"xfs", "s3"} {
		for _, failure := range []string{"", "node", "timeout", "nil", "nil-ret", "complete"} {
			t.Run(backend+failure, func(t *testing.T) {
				p := lifecycleBuffer(t)
				patchPauseDependencies(t, failure, backend)
				rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
				if failure != "" {
					require.NotEqual(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
					require.Empty(t, p.Events())
				} else {
					require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
					readLifecycleEvent(t, p, webhook.TypePaused)
				}
			})
		}
	}
}
func patchResumeDependencies(t *testing.T, failure string) {
	p := gomonkey.NewPatches()
	t.Cleanup(p.Reset)
	p.ApplyFunc(pausesnap.GetBySandbox, func(context.Context, string) (*pausesnap.Record, error) {
		return &pausesnap.Record{SnapshotID: "snap", Status: "READY", NodeIP: "127.0.0.1", Backend: "xfs"}, nil
	})
	p.ApplyFunc(sandboxspec.Get, func(context.Context, string) (*types.CreateCubeSandboxReq, error) { return lifecycleSpec(), nil })
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
func TestResumeFinalBoundary(t *testing.T) {
	for _, failure := range []string{"", "node", "identity", "proxy", "purge"} {
		t.Run(failure, func(t *testing.T) {
			p := lifecycleBuffer(t)
			patchResumeDependencies(t, failure)
			rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			if failure != "" {
				require.NotEqual(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				require.Empty(t, p.Events())
			} else {
				require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				readLifecycleEvent(t, p, webhook.TypeResumed)
			}
		})
	}
}

func TestPauseResumeRuntimeHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, webhook.TypeCreated, webhook.TypeResumed, webhook.TypePaused)
	SetWebhookPublisher(r.Publisher())
	t.Cleanup(func() { SetWebhookPublisher(nil) })
	c := createLifecycleFixture(t, context.Background(), nil)
	c.dealSuccResult()
	t.Run("pause", func(t *testing.T) {
		patchPauseDependencies(t, "", "s3")
		rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	})
	t.Run("resume", func(t *testing.T) {
		patchResumeDependencies(t, "")
		rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
		require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
	})
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
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
	require.Len(t, seen, 2)
}

func TestCreateCancellationDuringPostProcessing(t *testing.T) {
	p := lifecycleBuffer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := createLifecycleFixture(t, ctx, nil)
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

type resumeTimeoutProvider struct{ seen []webhook.Origin }

func (p *resumeTimeoutProvider) RefreshTimeout(ctx context.Context, _ string, _ int) (int64, error) {
	p.seen = append(p.seen, webhook.OriginFromContext(ctx))
	return 100, nil
}
func (p *resumeTimeoutProvider) LookupEndAt(context.Context, string) (int64, error) { return 0, nil }
func TestResumeTimeoutOrigin(t *testing.T) {
	p := &resumeTimeoutProvider{}
	old := getTimeoutProvider()
	SetTimeoutProvider(p)
	defer SetTimeoutProvider(old)
	ctx := context.Background()
	for _, value := range []int{0, 5, -1} {
		v := value
		publishUpdateTimeout(ctx, &types.UpdateRequest{Action: "resume", SandboxID: "sb", Timeout: &v})
	}
	publishUpdateTimeout(ctx, &types.UpdateRequest{Action: "resume", SandboxID: "sb"})
	require.Equal(t, []webhook.Origin{webhook.OriginLifecycle, webhook.OriginLifecycle}, p.seen)
	require.Equal(t, webhook.OriginLifecycle, webhook.OriginFromContext(ctx))
}
func TestFailoverEnqueuesOrigin(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	f := setupDestroyFixture(t)
	lifecycleBuffer(t)
	c := createLifecycleFixture(t, context.Background(), nil)
	c.masterRsp.SandboxID = "sb"
	c.setMasterRsp(int(errorcode.ErrorCode_DBError), "fail")
	c.failover()
	require.NotNil(t, f.pending)
	require.Equal(t, webhook.OriginCreateFailover, webhook.OriginFromContext(f.pending.Ctx))
	require.Equal(t, task.DestroySandbox, f.pending.TaskType)
}

func TestPauseResumeNotificationFailureIsolation(t *testing.T) {
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		c := createLifecycleFixture(t, context.Background(), nil)
		c.dealSuccResult()
		require.Equal(t, int(errorcode.ErrorCode_Success), c.masterRsp.Ret.RetCode)
		t.Run("pause", func(t *testing.T) {
			patchPauseDependencies(t, "", "s3")
			rsp := pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		})
		t.Run("resume", func(t *testing.T) {
			patchResumeDependencies(t, "")
			rsp := resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1")
			require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
		})
	})
}

func TestUpdateDetachedCancellationAndResumeTimeout(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		for _, timeout := range []*int{nil, types.TimeoutPtr(0), types.TimeoutPtr(5), types.TimeoutPtr(-1)} {
			t.Run(action, func(t *testing.T) {
				buffer := lifecycleBuffer(t)
				if action == "pause" {
					patchPauseDependencies(t, "", "s3")
				} else {
					patchResumeDependencies(t, "")
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
				provider := &resumeTimeoutProvider{}
				old := getTimeoutProvider()
				SetTimeoutProvider(provider)
				defer SetTimeoutProvider(old)
				rsp := Update(ctx, &types.UpdateRequest{RequestID: "req", SandboxID: "sb", InstanceType: "cubebox", Action: action, Timeout: timeout})
				require.Equal(t, int(errorcode.ErrorCode_Success), rsp.Ret.RetCode)
				kind := webhook.TypePaused
				if action == "resume" {
					kind = webhook.TypeResumed
				}
				readLifecycleEvent(t, buffer, kind)
				if action == "resume" && timeout != nil && *timeout != 0 {
					require.Equal(t, []webhook.Origin{webhook.OriginLifecycle}, provider.seen)
				} else {
					require.Empty(t, provider.seen)
				}
			})
		}
	}
}

func TestPauseDuplicateAndUploadResult(t *testing.T) {
	for _, terminal := range []string{constants.RemoteStatusReady, constants.RemoteStatusFailed} {
		t.Run(terminal, func(t *testing.T) {
			db := webhooktest.SnapshotDB(t)
			pausesnap.Init(db)
			defer pausesnap.Init(nil)
			p := lifecycleBuffer(t)
			patchPauseDependencies(t, "real-store", "s3")
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
			readLifecycleEvent(t, p, webhook.TypePaused)
		})
	}
}

func TestPauseTimeoutHealingDoesNotPublish(t *testing.T) {
	db := webhooktest.SnapshotDB(t)
	pausesnap.Init(db)
	defer pausesnap.Init(nil)
	p := lifecycleBuffer(t)
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

func TestDisabledRuntimeSkipsLifecycleProducerPreparation(t *testing.T) {
	if webhooktest.Isolated(t) {
		return
	}
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			cfg := config.DefaultWebhookConf()
			cfg.Invalid, cfg.Enabled = invalid, invalid
			r, err := webhook.NewRuntime(context.Background(), cfg)
			if invalid {
				require.ErrorIs(t, err, config.ErrWebhookConfig)
			} else {
				require.NoError(t, err)
			}
			defer r.Shutdown(context.Background())
			SetWebhookPublisher(r.Publisher())
			task.SetWebhookPublisher(r.Publisher())
			defer SetWebhookPublisher(nil)
			defer task.SetWebhookPublisher(nil)
			before := webhooktest.CaptureAccepted(t)
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(webhook.PrepareLifecycle, func(*types.CreateCubeSandboxReq) (string, webhook.LifecycleFields) {
				panic("disabled lifecycle fields")
			})
			patches.ApplyFunc(webhook.UserMetadata, func(map[string]string) webhook.LifecycleFields { panic("disabled metadata") })
			patches.ApplyFunc(webhook.WithOrigin, func(context.Context, webhook.Origin) context.Context { panic("disabled origin context") })
			patches.ApplyFunc(webhook.WithKillReason, func(context.Context, string) context.Context { panic("disabled reason context") })
			patches.ApplyFunc(webhook.Capture, func(webhook.CaptureInput) (*webhook.CapturedEvent, error) { panic("disabled Capture/UUID path") })
			t.Run("pause", func(t *testing.T) {
				patchPauseDependencies(t, "", "xfs")
				require.Equal(t, 200, pauseSandbox(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1").Ret.RetCode)
			})
			t.Run("resume", func(t *testing.T) {
				patchResumeDependencies(t, "")
				require.Equal(t, 200, resumeFromPauseSnapshot(context.Background(), &types.UpdateRequest{SandboxID: "sb", InstanceType: "cubebox"}, "127.0.0.1").Ret.RetCode)
			})
			t.Run("updates-and-destroy", func(t *testing.T) {
				f := setupDestroyFixture(t)
				old := getTimeoutProvider()
				defer SetTimeoutProvider(old)
				SetTimeoutProvider(timeoutFixture{1700000000123, nil})
				require.Equal(t, 200, SetTimeout(context.Background(), &types.SetTimeoutRequest{SandboxID: "sb", Timeout: 5}).Ret.RetCode)
				require.Equal(t, 200, Refresh(context.Background(), &types.RefreshSandboxRequest{SandboxID: "sb", Duration: 5}).Ret.RetCode)
				require.Equal(t, 200, UpdateNetwork(context.Background(), networkFixture()).Ret.RetCode)
				for _, sync := range []bool{true, false} {
					require.Equal(t, 200, DestroySandbox(context.Background(), &types.DeleteCubeSandboxReq{SandboxID: "sb", InstanceType: "cubebox", Sync: sync}).Ret.RetCode)
					if !sync {
						require.NoError(t, (&task.DestroySandboxTaskHandler{}).HandleTask(f.pending.Ctx, f.pending))
					}
				}
				require.Equal(t, 3, f.nodeCalls)
				require.Equal(t, 3, f.specReads)
			})
			t.Run("failover", func(t *testing.T) {
				f := setupDestroyFixture(t)
				c := createLifecycleFixture(t, context.Background(), nil)
				c.masterRsp.SandboxID = "sb"
				c.setMasterRsp(int(errorcode.ErrorCode_DBError), "fail")
				c.failover()
				require.NotNil(t, f.pending)
				require.Equal(t, webhook.OriginLifecycle, webhook.OriginFromContext(f.pending.Ctx))
			})
			require.Equal(t, before, webhooktest.CaptureAccepted(t))
		})
	}
}

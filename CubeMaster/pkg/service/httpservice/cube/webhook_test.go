// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// Uses the public HTTP handler and its existing template/run seams. The event
// belongs to the final returned result, never to a background create worker.
func createWebhookFixture(t *testing.T, kind, failure string) interface{} {
	t.Helper()
	oldDeal, oldRun := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn
	defer func() { createSandboxDealCubeboxCreateReqWithTemplateFn = oldDeal; createSandboxRunFn = oldRun }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
		calls++
		if failure == "template" {
			return errors.New("template failed")
		}
		r := templateResolveResultFromContext(ctx)
		r.TemplateID, r.Kind = "trusted-template", kind
		req.Annotations[constants.CubeAnnotationAppSnapshotTemplateAlias] = "trusted-alias"
		req.Labels["template-secret"] = "not-user"
		req.Labels["user_token"] = "template-overwrite"
		return nil
	}
	createSandboxRunFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
		calls++
		require.Equal(t, "template-overwrite", req.Labels["user_token"])
		code := int(errorcode.ErrorCode_Success)
		if failure == "create" {
			code = int(errorcode.ErrorCode_MasterInternalError)
		}
		id := "created-sandbox"
		if failure == "mock-shortcut" {
			id = ""
		}
		return &types.CreateCubeSandboxRes{SandboxID: id, Ret: &types.Ret{RetCode: code}}
	}
	labels := `{"user_token":"kept","host-mount":"private"}`
	if failure == "oversize" {
		labels = `{"user_token":"` + strings.Repeat("v", 4097) + `"}`
	}
	r := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"requestID":"r","labels":`+labels+`,"annotations":{"cube.master.runtime.snapshot.id":"snap-forged"},"_webhook_metadata":{"values":{"spoof":"bad"}}}`)).WithContext(ctx)
	result := createSandbox(r, &CubeLog.RequestTrace{})
	if failure == "template" {
		require.Equal(t, 1, calls)
	} else {
		require.Equal(t, 2, calls)
	}
	return result
}

func TestCreateWebhookUsesTrustedFinalResult(t *testing.T) {
	for _, kind := range []string{"template", "snapshot", ""} {
		for _, failure := range []string{"", "template", "create", "mock-shortcut", "oversize"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				p, err := webhook.NewBufferedPublisher(4)
				require.NoError(t, err)
				sandbox.SetWebhookPublisher(p)
				defer sandbox.SetWebhookPublisher(nil)
				result := createWebhookFixture(t, kind, failure)
				if failure != "" {
					require.Empty(t, p.Events())
					return
				}
				require.Equal(t, 200, result.(*types.CreateCubeSandboxRes).Ret.RetCode)
				require.Equal(t, "trusted-alias", result.(*types.CreateCubeSandboxRes).ExtInfo[constants.CubeAnnotationAppSnapshotTemplateAlias])
				require.Len(t, p.Events(), 1)
				captured := <-p.Events()
				data, err := webhook.MapToProtocol(captured)
				require.NoError(t, err)
				event, err := webhook.Encode(captured, data)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, json.Unmarshal(event.Body(), &body))
				want := webhook.TypeCreated
				if kind == "snapshot" {
					want = webhook.TypeResumed
					require.Equal(t, "restore", data.ResumeMode)
				}
				require.Equal(t, want, event.Type())
				require.Equal(t, "trusted-template", body["sandbox_template_id"])
				require.Equal(t, map[string]string{"user_token": "kept"}, data.SandboxMetadata)
			})
		}
	}
}

func TestCreateWebhookDisabledRuntimeSkipsPreparation(t *testing.T) {
	for _, c := range []config.WebhookConf{config.DefaultWebhookConf(), {Enabled: true, Invalid: true}} {
		r, err := webhook.NewRuntime(context.Background(), c)
		if c.Invalid {
			require.ErrorIs(t, err, config.ErrWebhookConfig)
		} else {
			require.NoError(t, err)
		}
		defer r.Shutdown(context.Background())
		sandbox.SetWebhookPublisher(r.Publisher())
		defer sandbox.SetWebhookPublisher(nil)
		before := webhooktest.CaptureAccepted(t)
		patches := gomonkey.NewPatches()
		patches.ApplyFunc(webhook.UserMetadata, func(map[string]string) webhook.LifecycleFields { panic("disabled metadata preparation") })
		patches.ApplyFunc(webhook.PrepareLifecycle, func(*types.CreateCubeSandboxReq) (string, webhook.LifecycleFields) {
			panic("disabled lifecycle preparation")
		})
		patches.ApplyFunc(webhook.Capture, func(webhook.CaptureInput) (*webhook.CapturedEvent, error) { panic("disabled capture") })
		result := createWebhookFixture(t, "snapshot", "")
		patches.Reset()
		require.Equal(t, 200, result.(*types.CreateCubeSandboxRes).Ret.RetCode)
		require.Equal(t, before, webhooktest.CaptureAccepted(t))
	}
}

func TestCreateWebhookMetadataBudget(t *testing.T) {
	for _, kind := range []string{"template", "snapshot"} {
		for _, annotation := range []bool{false, true} {
			for _, count := range []int{61, 62, 63, 64, 65} {
				name := kind + "/labels-" + strconv.Itoa(count) + "/template-annotation-" + strconv.FormatBool(annotation)
				t.Run(name, func(t *testing.T) {
					oldDeal, oldRun, oldRegister := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn
					defer func() {
						createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn = oldDeal, oldRun, oldRegister
					}()
					p, err := webhook.NewBufferedPublisher(2)
					require.NoError(t, err)
					sandbox.SetWebhookPublisher(p)
					defer sandbox.SetWebhookPublisher(nil)
					labels := make(map[string]string, count)
					for i := 0; i < count; i++ {
						labels["user_"+strconv.Itoa(i)] = "value"
					}
					var calls []string
					createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
						calls = append(calls, "template")
						added := 2
						if annotation {
							added++
						}
						require.Len(t, req.Labels, count+added)
						require.Equal(t, "business-caller", req.Labels[constants.Caller])
						require.Equal(t, "cubebox", req.Labels[constants.CubeAnnotationsInsType])
						resolved := templateResolveResultFromContext(ctx)
						resolved.TemplateID, resolved.Kind = "trusted-template", kind
						req.Labels["template-secret"] = "not-user"
						req.Labels["user_0"] = "template-overwrite"
						return nil
					}
					createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
						calls = append(calls, "create")
						return &types.CreateCubeSandboxRes{SandboxID: "metadata-budget", Ret: &types.Ret{RetCode: 200}}
					}
					createSandboxRegisterRuntimeRefFn = func(context.Context, string, string, string, string) error {
						calls = append(calls, "runtime-ref")
						return nil
					}
					input := map[string]any{"requestID": "metadata-budget", "labels": labels}
					if annotation {
						input["annotations"] = map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "trusted-template"}
					}
					body, err := json.Marshal(input)
					require.NoError(t, err)
					request := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(string(body)))
					request.Header.Set(constants.Caller, "business-caller")
					result := createSandbox(request, &CubeLog.RequestTrace{}).(*types.CreateCubeSandboxRes)
					require.Equal(t, 200, result.Ret.RetCode)
					wantCalls := []string{"template", "create"}
					if kind == "snapshot" && annotation {
						wantCalls = append(wantCalls, "runtime-ref")
					}
					require.Equal(t, wantCalls, calls)
					if count > 64 {
						require.Empty(t, p.Events())
						return
					}
					require.Len(t, p.Events(), 1)
					captured := <-p.Events()
					data, err := webhook.MapToProtocol(captured)
					require.NoError(t, err)
					require.Equal(t, labels, data.SandboxMetadata)
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
}

func TestCreateWebhookHTTPRetryAndFailureIsolation(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, webhook.TypeCreated, webhook.TypeResumed)
	sandbox.SetWebhookPublisher(r.Publisher())
	defer sandbox.SetWebhookPublisher(nil)
	for _, kind := range []string{"template", "snapshot"} {
		require.Equal(t, 200, createWebhookFixture(t, kind, "").(*types.CreateCubeSandboxRes).Ret.RetCode)
		select {
		case <-bodies:
		case <-time.After(8 * time.Second):
			t.Fatal("public create notification not delivered")
		}
	}
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		sandbox.SetWebhookPublisher(p)
		defer sandbox.SetWebhookPublisher(nil)
		require.Equal(t, 200, createWebhookFixture(t, "template", "").(*types.CreateCubeSandboxRes).Ret.RetCode)
	})
}

func TestDeleteWebhookDoesNotRewriteBusinessReason(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var publisher webhook.Publisher
		if enabled {
			p, err := webhook.NewBufferedPublisher(1)
			require.NoError(t, err)
			publisher = p
		} else {
			r, err := webhook.NewRuntime(context.Background(), config.DefaultWebhookConf())
			require.NoError(t, err)
			defer r.Shutdown(context.Background())
			publisher = r.Publisher()
		}
		sandbox.SetWebhookPublisher(publisher)
		defer sandbox.SetWebhookPublisher(nil)
		patches := gomonkey.ApplyFunc(sandbox.DestroySandbox, func(ctx context.Context, req *types.DeleteCubeSandboxReq) *types.DeleteCubeSandboxRes {
			require.Empty(t, req.KillReason, "HTTP delete must retain the original request")
			want := ""
			if enabled {
				want = "request"
			}
			require.Equal(t, want, webhook.KillReasonFromContext(ctx))
			return &types.DeleteCubeSandboxRes{Ret: &types.Ret{RetCode: 200}}
		})
		r := httptest.NewRequest("DELETE", "/cube/sandbox", strings.NewReader(`{"requestID":"delete","sandbox_id":"sb","instance_type":"cubebox"}`))
		deleteSandbox(r, &CubeLog.RequestTrace{})
		patches.Reset()
	}
}

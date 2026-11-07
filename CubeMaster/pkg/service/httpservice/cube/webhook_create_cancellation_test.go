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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// The producer follows the final business result. Cancellation after success
// must not suppress a success event; a returned cancellation must publish none.
// Business CreateSandbox/Wait arbitration is outside this webhook test's scope.
func TestCreateWebhookFinalResultWithCancellation(t *testing.T) {
	for _, kind := range []string{"template", "snapshot"} {
		for _, outcome := range []string{"success", "cancelled-result", "success-before-cancel"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p, err := webhook.NewBufferedPublisher(4)
				require.NoError(t, err)
				sandbox.SetWebhookPublisher(p)
				defer sandbox.SetWebhookPublisher(nil)
				oldDeal, oldRun, oldRegister := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn
				defer func() {
					createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn, createSandboxRegisterRuntimeRefFn = oldDeal, oldRun, oldRegister
				}()
				patches := gomonkey.NewPatches()
				defer patches.Reset()
				patches.ApplyFunc(config.GetConfig, func() *config.Config { return &config.Config{} })
				createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
					resolved := templateResolveResultFromContext(ctx)
					resolved.TemplateID, resolved.Kind = "trusted-template", kind
					req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = "trusted-template"
					req.Annotations[constants.CubeAnnotationAppSnapshotTemplateAlias] = "trusted-alias"
					return nil
				}
				refCalls, runCalls := 0, 0
				createSandboxRegisterRuntimeRefFn = func(context.Context, string, string, string, string) error { refCalls++; return nil }
				code := int(errorcode.ErrorCode_Success)
				if outcome == "cancelled-result" {
					code = int(errorcode.ErrorCode_ClientCancel)
				}
				createSandboxRunFn = func(context.Context, *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
					runCalls++
					result := &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: code}, SandboxID: "final-result"}
					if outcome != "success" {
						cancel() // The outer producer sees a cancelled context and this final result.
					}
					return result
				}
				body := `{"requestID":"final-result"}`
				request := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(body)).WithContext(ctx)
				result := createSandbox(request, &CubeLog.RequestTrace{}).(*types.CreateCubeSandboxRes)
				require.Equal(t, 1, runCalls)
				require.Equal(t, code, result.Ret.RetCode)
				if outcome == "success" {
					require.NoError(t, ctx.Err())
				} else {
					require.ErrorIs(t, ctx.Err(), context.Canceled)
				}
				if outcome == "cancelled-result" {
					require.Zero(t, refCalls)
					require.Empty(t, p.Events(), "a failed business result publishes no lifecycle success")
					return
				}
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

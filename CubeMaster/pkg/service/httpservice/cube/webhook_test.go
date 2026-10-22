// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP4TrustedResolveAndPreMergeMetadata(t *testing.T) {
	oldDeal, oldRun := createSandboxDealCubeboxCreateReqWithTemplateFn, createSandboxRunFn
	defer func() { createSandboxDealCubeboxCreateReqWithTemplateFn = oldDeal; createSandboxRunFn = oldRun }()
	for _, kind := range []string{"template", "snapshot", ""} {
		t.Run(kind, func(t *testing.T) {
			createSandboxDealCubeboxCreateReqWithTemplateFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) error {
				r := templateResolveResultFromContext(ctx)
				r.TemplateID = "snap-trusted"
				r.Kind = kind
				req.Labels["template-secret"] = "not-user"
				req.Labels["user_token"] = "template-overwrite"
				return nil
			}
			createSandboxRunFn = func(ctx context.Context, req *types.CreateCubeSandboxReq) *types.CreateCubeSandboxRes {
				template, snapshot := webhook.ResolvedStart(ctx)
				require.Equal(t, "snap-trusted", template)
				require.Equal(t, kind == "snapshot", snapshot)
				require.Equal(t, map[string]string{"user_token": "kept"}, req.WebhookMetadata.Values)
				return &types.CreateCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterInternalError)}}
			}
			r := httptest.NewRequest("POST", "/cube/sandbox", strings.NewReader(`{"requestID":"r","labels":{"user_token":"kept","host-mount":"private"},"annotations":{"cube.master.runtime.snapshot.id":"snap-forged"},"_webhook_metadata":{"values":{"spoof":"bad"}}}`))
			createSandbox(r, &CubeLog.RequestTrace{})
		})
	}
}

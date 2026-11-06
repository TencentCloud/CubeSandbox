// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func stubAdoptJFSTemplate(t *testing.T, fn func(ctx context.Context, templateID string) (bool, error)) {
	t.Helper()
	orig := adoptJFSTemplate
	adoptJFSTemplate = fn
	t.Cleanup(func() { adoptJFSTemplate = orig })
}

func TestAdoptJFSTemplateRPCSuccess(t *testing.T) {
	stubAdoptJFSTemplate(t, func(_ context.Context, id string) (bool, error) {
		require.Equal(t, "tpl-1", id)
		return true, nil
	})
	rsp, err := (&service{}).AdoptJFSTemplate(context.Background(), &cubeboxv1.AdoptJFSTemplateRequest{RequestID: "r-1", TemplateID: "tpl-1"})
	require.NoError(t, err)
	require.Equal(t, int(errorcodev1.ErrorCode_Success), int(rsp.GetRet().GetRetCode()))
	require.True(t, rsp.GetAdopted())
	require.Equal(t, "tpl-1", rsp.GetTemplateID())
}

func TestAdoptJFSTemplateRPCIdempotentNoOp(t *testing.T) {
	stubAdoptJFSTemplate(t, func(_ context.Context, id string) (bool, error) {
		return false, nil
	})
	rsp, err := (&service{}).AdoptJFSTemplate(context.Background(), &cubeboxv1.AdoptJFSTemplateRequest{TemplateID: "tpl-1"})
	require.NoError(t, err)
	require.Equal(t, int(errorcodev1.ErrorCode_Success), int(rsp.GetRet().GetRetCode()))
	require.False(t, rsp.GetAdopted())
}

func TestAdoptJFSTemplateRPCValidation(t *testing.T) {
	stubAdoptJFSTemplate(t, func(_ context.Context, id string) (bool, error) {
		t.Fatal("invalid requests must not reach storage")
		return false, nil
	})
	for _, tc := range []struct {
		name       string
		templateID string
	}{
		{"empty", "  "},
		{"path escape", "../evil"},
		{"separator", "a/b"},
	} {
		rsp, err := (&service{}).AdoptJFSTemplate(context.Background(), &cubeboxv1.AdoptJFSTemplateRequest{TemplateID: tc.templateID})
		require.NoError(t, err, tc.name)
		require.Equal(t, int(errorcodev1.ErrorCode_InvalidParamFormat), int(rsp.GetRet().GetRetCode()), tc.name)
	}
}

func TestAdoptJFSTemplateRPCStorageError(t *testing.T) {
	stubAdoptJFSTemplate(t, func(_ context.Context, id string) (bool, error) {
		return false, errors.New("template is not shared-ready")
	})
	rsp, err := (&service{}).AdoptJFSTemplate(context.Background(), &cubeboxv1.AdoptJFSTemplateRequest{TemplateID: "tpl-1"})
	require.NoError(t, err)
	require.Equal(t, int(errorcodev1.ErrorCode_Unknown), int(rsp.GetRet().GetRetCode()))
	require.Contains(t, rsp.GetRet().GetRetMsg(), "shared-ready")
}

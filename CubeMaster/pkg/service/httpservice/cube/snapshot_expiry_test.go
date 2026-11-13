// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/snapshotmode"
)

func TestCreateSnapshotPassesTTLSeconds(t *testing.T) {
	registerKnownSandboxTestID(t)

	origCreateSnapshotFn := createSnapshotFn
	origGetSnapshotInfoFn := getSnapshotInfoFn
	origResolveSnapshotHostFn := resolveSnapshotHostFn
	t.Cleanup(func() {
		createSnapshotFn = origCreateSnapshotFn
		getSnapshotInfoFn = origGetSnapshotInfoFn
		resolveSnapshotHostFn = origResolveSnapshotHostFn
	})
	resolveSnapshotHostFn = func(context.Context, string, string) (string, string, error) {
		return "node-a", "10.0.0.1", nil
	}
	createSnapshotFn = func(_ context.Context, _, _, _, _, _, _ string, _ snapshotmode.Mode, ttlSeconds int64) (*types.TemplateImageJobInfo, error) {
		assert.Equal(t, int64(60), ttlSeconds)
		return &types.TemplateImageJobInfo{JobID: "op-ttl", TemplateID: "snap-ttl"}, nil
	}
	getSnapshotInfoFn = func(context.Context, string, bool) (*templatecenter.SnapshotInfo, error) {
		return &templatecenter.SnapshotInfo{SnapshotID: "snap-ttl"}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-ttl",
		"sandbox_id":"`+knownSandboxTestID+`",
		"ttl_seconds":60
	}`))
	resp := createSnapshot(req, &CubeLog.RequestTrace{}).(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_Success), resp.Ret.RetCode)
}

func TestCreateSnapshotRejectsNegativeTTLSeconds(t *testing.T) {
	registerKnownSandboxTestID(t)

	req := httptest.NewRequest(http.MethodPost, "/cube/snapshot", strings.NewReader(`{
		"request_id":"req-negative-ttl",
		"sandbox_id":"`+knownSandboxTestID+`",
		"ttl_seconds":-1
	}`))
	resp := createSnapshot(req, &CubeLog.RequestTrace{}).(*snapshotResponse)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), resp.Ret.RetCode)
	assert.Equal(t, "ttl_seconds must be greater than or equal to zero", resp.Ret.RetMsg)
}

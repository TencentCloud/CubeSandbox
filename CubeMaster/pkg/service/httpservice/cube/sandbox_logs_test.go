// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// invokeLogsHandler drives handleSandboxLogsAction with stubbed ID resolution,
// host lookup and Cubelet RPC; returns the response and captured request.
func invokeLogsHandler(t *testing.T, body string, hostIP string, fromPause bool,
	rsp *cubebox.GetSandboxEventsResponse, rpcErr error) (SandboxLogsRes, *cubebox.GetSandboxEventsRequest) {
	t.Helper()

	var gotReq *cubebox.GetSandboxEventsRequest
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(sandbox.ResolveSandboxID, func(_ context.Context, sandboxID string) (string, error) {
		return sandboxID, nil
	})
	patches.ApplyFunc(sandbox.ResolveEventHostIP, func(context.Context, string) (string, bool) {
		return hostIP, fromPause
	})
	patches.ApplyFunc(cubelet.GetSandboxEvents, func(_ context.Context, _ string, req *cubebox.GetSandboxEventsRequest) (*cubebox.GetSandboxEventsResponse, error) {
		gotReq = req
		return rsp, rpcErr
	})

	req := httptest.NewRequest("POST", "/cube/sandbox/logs", strings.NewReader(body))
	ctx := CubeLog.WithRequestTrace(context.Background(), &CubeLog.RequestTrace{})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req.WithContext(ctx)
	handleSandboxLogsAction(c)

	var got SandboxLogsRes
	require.NoError(t, common.FastestJsoniter.Unmarshal(w.Body.Bytes(), &got))
	return got, gotReq
}

func TestHandleSandboxLogsActionForwardsCursorTailAndLimit(t *testing.T) {
	rsp := &cubebox.GetSandboxEventsResponse{
		Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		Events: []*cubebox.SandboxEvent{{
			Timestamp: "2026-07-22T10:00:00Z",
			Message:   "hello",
			Level:     "info",
			Module:    "Shim",
		}},
		NextCursor: 1784695200000,
		HasMore:    true,
	}

	got, gotReq := invokeLogsHandler(t,
		`{"sandboxID":"sb-1","cursor":1000,"limit":9999}`, "10.0.0.1", false, rsp, nil)

	assert.Equal(t, 0, got.Ret.RetCode)
	// limit is capped at maxLogLimit; cursor and tail flow through.
	assert.Equal(t, int32(maxLogLimit), gotReq.GetLimit())
	assert.Equal(t, int64(1000), gotReq.GetCursor())
	assert.False(t, gotReq.GetTail())
	// Response carries entries plus pagination fields and module.
	require.Len(t, got.Logs, 1)
	assert.Equal(t, "Shim", got.Logs[0].Module)
	assert.Equal(t, int64(1784695200000), got.NextCursor)
	assert.True(t, got.HasMore)
}

func TestHandleSandboxLogsActionRejectsTailWithCursor(t *testing.T) {
	got, gotReq := invokeLogsHandler(t,
		`{"sandboxID":"sb-1","tail":true,"cursor":1000}`, "10.0.0.1", false, nil, nil)

	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Ret.RetCode)
	assert.Contains(t, got.Ret.RetMsg, "mutually exclusive")
	assert.Nil(t, gotReq)
}

func TestHandleSandboxLogsActionUnresolvableHostIsRetryable(t *testing.T) {
	got, gotReq := invokeLogsHandler(t, `{"sandboxID":"sb-1"}`, "", false, nil, nil)

	assert.Equal(t, int(errorcode.ErrorCode_ConnHostFailed), got.Ret.RetCode)
	assert.Nil(t, gotReq)
}

func TestHandleSandboxLogsActionPausesnapRoutingStillServes(t *testing.T) {
	rsp := &cubebox.GetSandboxEventsResponse{
		Ret:    &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success},
		Events: []*cubebox.SandboxEvent{},
	}

	got, gotReq := invokeLogsHandler(t, `{"sandboxID":"sb-1","tail":true}`, "10.0.0.2", true, rsp, nil)

	assert.Equal(t, 0, got.Ret.RetCode)
	assert.NotNil(t, gotReq)
	assert.True(t, gotReq.GetTail())
}

func TestClassifyCubeletEventError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode errorcode.ErrorCode
		wantMsg  string
	}{
		{
			name:     "wrapped ret error keeps its code",
			err:      ret.Err(errorcode.ErrorCode_ConnHostFailed, "dial tcp 10.0.0.1:7559: refused"),
			wantCode: errorcode.ErrorCode_ConnHostFailed,
			wantMsg:  "failed to fetch sandbox events from cubelet",
		},
		{
			name:     "unimplemented reports upgrade order",
			err:      status.Error(codes.Unimplemented, "unknown method"),
			wantCode: errorcode.ErrorCode_MasterInternalError,
			wantMsg:  "cubelet does not support GetSandboxEvents; upgrade cubelet on the owning node first",
		},
		{
			name:     "unavailable stays retryable",
			err:      status.Error(codes.Unavailable, "transport closing"),
			wantCode: errorcode.ErrorCode_ConnHostFailed,
			wantMsg:  "cubelet unreachable",
		},
		{
			name:     "deadline stays retryable",
			err:      status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			wantCode: errorcode.ErrorCode_ConnHostFailed,
			wantMsg:  "cubelet unreachable",
		},
		{
			name:     "other grpc errors are internal",
			err:      status.Error(codes.Internal, "boom"),
			wantCode: errorcode.ErrorCode_MasterInternalError,
			wantMsg:  "failed to fetch sandbox events from cubelet",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := classifyCubeletEventError(tc.err)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantMsg, msg)
			assert.NotContains(t, msg, "10.0.0.1")
		})
	}
}

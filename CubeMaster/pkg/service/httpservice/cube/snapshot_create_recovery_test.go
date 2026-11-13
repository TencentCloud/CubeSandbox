// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func TestRecoverSnapshotCreateReservationHTTP(t *testing.T) {
	old := recoverSnapshotCreateReservationFn
	t.Cleanup(func() { recoverSnapshotCreateReservationFn = old })
	for _, failure := range []error{nil, templatecenter.ErrTemplateInUse} {
		recoverSnapshotCreateReservationFn = func(_ context.Context, id string, req templatecenter.SnapshotCreateRecovery) error {
			require.Equal(t, "source", id)
			require.Equal(t, "reserved", req.SandboxID)
			require.Equal(t, "node", req.NodeID)
			require.Equal(t, "192.0.2.10", req.NodeIP)
			require.True(t, req.ConfirmNodeFenced)
			require.Equal(t, "powered off", req.Reason)
			return failure
		}
		trace := &CubeLog.RequestTrace{}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest("POST", "/cube/snapshot/source/recover-create", strings.NewReader(`{"sandbox_id":"reserved","node_id":"node","node_ip":"192.0.2.10","confirm_node_fenced":true,"reason":"powered off"}`)).WithContext(CubeLog.WithRequestTrace(context.Background(), trace))
		ctx.Params = gin.Params{{Key: "snapshot_id", Value: "source"}}
		recoverSnapshotCreateReservationGinHandler(ctx)
		var got types.Res
		require.NoError(t, common.FastestJsoniter.Unmarshal(recorder.Body.Bytes(), &got))
		want := int(errorcode.ErrorCode_Success)
		if failure != nil {
			want = int(errorcode.ErrorCode_Conflict)
		}
		require.Equal(t, want, got.Ret.RetCode)
		require.EqualValues(t, want, trace.RetCode)
	}
}

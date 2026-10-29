// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/grpc/metadata"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// An empty SandboxID makes Update reject before touching any storage.
func pauseReq(requestID string) *cubeboxv1.UpdateCubeSandboxRequest {
	return &cubeboxv1.UpdateCubeSandboxRequest{
		RequestID: requestID,
		Annotations: map[string]string{
			constants.MasterAnnotationsUpdateAction: constants.UpdateActionPause,
		},
	}
}

func TestUpdatePauseEarlyFailureIsTraced(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)
	svc := createServiceForTest(nil, &fakeCreateFlow{}, 0)

	rsp, err := svc.Update(context.Background(), pauseReq("req-pause-early-fail"))
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_InvalidParamFormat, rsp.GetRet().GetRetCode())
	flush()

	spans := rec.named(telemetry.SpanPause)
	require.Len(t, spans, 1, "the pause attempt must open exactly one root span")
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	got, ok := retCodeAttr(spans[0])
	require.True(t, ok, "the root span must carry %s", telemetry.AttrRetCode)
	assert.Equal(t, int(errorcode.ErrorCode_InvalidParamFormat), got)
}

func TestUpdateNonPauseActionOpensNoRootSpan(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)
	svc := createServiceForTest(nil, &fakeCreateFlow{}, 0)

	rsp, err := svc.Update(context.Background(), &cubeboxv1.UpdateCubeSandboxRequest{
		RequestID:   "req-network",
		Annotations: map[string]string{constants.MasterAnnotationsUpdateAction: "network"},
	})
	require.NoError(t, err)
	require.NotNil(t, rsp)
	flush()

	assert.Empty(t, rec.named(telemetry.SpanPause),
		"a non-pause action must not open the pause root span")
}

func TestUpdatePauseRootSpanAdoptsIncomingTraceparent(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)
	svc := createServiceForTest(nil, &fakeCreateFlow{}, 0)

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("traceparent", rollbackTraceparent))

	rsp, err := svc.Update(ctx, pauseReq("req-pause-traceparent"))
	require.NoError(t, err)
	require.NotNil(t, rsp)
	flush()

	spans := rec.named(telemetry.SpanPause)
	require.Len(t, spans, 1)
	parent := spans[0].Parent()
	require.True(t, parent.IsValid(), "pause must continue the caller's trace")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", parent.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", parent.SpanID().String())
}

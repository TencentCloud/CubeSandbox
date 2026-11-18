// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type snapshotSpanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *snapshotSpanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *snapshotSpanRecorder) Shutdown(context.Context) error { return nil }

func (r *snapshotSpanRecorder) named(name string) []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sdktrace.ReadOnlySpan
	for _, s := range r.spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func setupSnapshotTracer(t *testing.T) (*snapshotSpanRecorder, func()) {
	t.Helper()
	rec := &snapshotSpanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cubelet-cubebox-test")
	require.NoError(t, err)
	var once sync.Once
	flush := func() {
		once.Do(func() { _ = shutdown(context.Background()) })
	}
	t.Cleanup(flush)
	return rec, flush
}

func retCodeAttr(span sdktrace.ReadOnlySpan) (int, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == telemetry.AttrRetCode {
			return int(kv.Value.AsInt64()), true
		}
	}
	return 0, false
}

func TestAppSnapshotEarlyFailuresAreTraced(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	validAnnotations := map[string]string{
		constants.MasterAnnotationsAppSnapshotCreate:    "true",
		constants.MasterAnnotationAppSnapshotTemplateID: "template",
	}
	cases := []struct {
		name string
		req  *cubeboxv1.AppSnapshotRequest
		want errorcode.ErrorCode
	}{
		{
			name: "nil create request",
			req:  &cubeboxv1.AppSnapshotRequest{},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "snapshot annotations rejected",
			req: &cubeboxv1.AppSnapshotRequest{CreateRequest: &cubeboxv1.RunCubeSandboxRequest{
				RequestID:   "req",
				Annotations: map[string]string{constants.MasterAnnotationsAppSnapshotCreate: "false"},
			}},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "cow backend disabled",
			req: &cubeboxv1.AppSnapshotRequest{CreateRequest: &cubeboxv1.RunCubeSandboxRequest{
				RequestID:   "req",
				Annotations: validAnnotations,
			}},
			want: errorcode.ErrorCode_PreConditionFailed,
		},
	}

	for _, tc := range cases {
		rsp, err := (&service{}).AppSnapshot(context.Background(), tc.req)
		require.NoError(t, err, tc.name)
		require.NotNil(t, rsp, tc.name)
		require.Equal(t, tc.want, rsp.GetRet().GetRetCode(), tc.name)
	}
	flush()

	spans := rec.named(telemetry.SpanImageAppSnapshot)
	require.Len(t, spans, len(cases), "one span per rejected request")
	for i, span := range spans {
		assert.Equal(t, codes.Error, span.Status().Code, cases[i].name)
		got, ok := retCodeAttr(span)
		require.True(t, ok, "%s: span carries no %s", cases[i].name, telemetry.AttrRetCode)
		assert.Equal(t, int(cases[i].want), got, cases[i].name)
	}
}

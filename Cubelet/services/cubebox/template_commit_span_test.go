// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/telemetry"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func TestTemplateCommitEarlyRejectionsAreTraced(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	cases := []struct {
		name string
		req  *cubeboxv1.CommitSandboxRequest
		want errorcode.ErrorCode
	}{
		{
			name: "empty template id",
			req:  &cubeboxv1.CommitSandboxRequest{SandboxID: "sb-1"},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "unsafe template id",
			req:  &cubeboxv1.CommitSandboxRequest{TemplateID: "bad/id", SandboxID: "sb-1"},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "empty sandbox id",
			req:  &cubeboxv1.CommitSandboxRequest{TemplateID: "tpl-1"},
			want: errorcode.ErrorCode_InvalidParamFormat,
		},
		{
			name: "cow storage disabled",
			req:  &cubeboxv1.CommitSandboxRequest{TemplateID: "tpl-1", SandboxID: "sb-1"},
			want: errorcode.ErrorCode_PreConditionFailed,
		},
	}

	for _, tc := range cases {
		rsp, err := (&service{}).CommitSandbox(context.Background(), tc.req)
		require.NoError(t, err, tc.name)
		require.NotNil(t, rsp, tc.name)
		require.Equal(t, tc.want, rsp.GetRet().GetRetCode(), tc.name)
	}
	flush()

	spans := rec.named(telemetry.SpanTemplateCommit)
	require.Len(t, spans, len(cases), "one span per rejected request")
	for i, span := range spans {
		assert.Equal(t, codes.Error, span.Status().Code, cases[i].name)
		got, ok := retCodeAttr(span)
		require.True(t, ok, "%s: span carries no %s", cases[i].name, telemetry.AttrRetCode)
		assert.Equal(t, int(cases[i].want), got, cases[i].name)
	}
}

func TestDetachedFrozenWorkContextCarriesCommitSpan(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	parent, cancel := context.WithCancel(context.Background())
	commitCtx, commitSpan := telemetry.Start(parent, telemetry.SpanTemplateCommit)
	defer commitSpan.End()

	frozenCtx, frozenCancel := detachedSnapshotWorkContext(commitCtx)
	defer frozenCancel()

	cancel()
	if frozenCtx.Err() != nil {
		t.Fatalf("frozen ctx cancelled with the request: %v", frozenCtx.Err())
	}

	_, phaseSpan := telemetry.Start(frozenCtx, telemetry.SpanTemplateCommitCapture)
	phaseSpan.End()
	flush()

	captures := rec.named(telemetry.SpanTemplateCommitCapture)
	require.Len(t, captures, 1)
	assert.Equal(t, commitSpan.SpanContext().SpanID(), captures[0].Parent().SpanID())
}

func TestEndCommitPhaseFinalizesOnPanic(t *testing.T) {
	rec, flush := setupSnapshotTracer(t)

	run := func(err error, panics bool) {
		func() {
			defer func() { _ = recover() }()
			_, span := telemetry.Start(context.Background(), telemetry.SpanTemplateCommitCapture)
			defer endCommitPhase(span, &err)
			if panics {
				panic("boom")
			}
		}()
	}

	run(nil, false)
	run(errors.New("phase failed"), false)
	run(nil, true)
	flush()

	spans := rec.named(telemetry.SpanTemplateCommitCapture)
	require.Len(t, spans, 3)
	assert.Equal(t, codes.Unset, spans[0].Status().Code, "nil error is success")
	assert.Equal(t, codes.Error, spans[1].Status().Code, "phase error is reported")
	assert.Equal(t, codes.Error, spans[2].Status().Code, "panicking phase is reported")
}

func TestS3ExportIncomplete(t *testing.T) {
	cases := []struct {
		backend string
		raw     string
		want    bool
	}{
		{"s3", "", true},
		{"s3", "uuid-1", false},
		{"xfs", "", false},
		{"", "", false},
		{"cubecow", "", false},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, s3ExportIncomplete(tc.backend, tc.raw), "backend=%q raw=%q", tc.backend, tc.raw)
	}
}

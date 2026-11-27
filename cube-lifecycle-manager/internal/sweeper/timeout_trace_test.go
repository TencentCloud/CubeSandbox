// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sweeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/telemetry"
)

func TestSweeperIdleActionTrace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retCode int
		status  codes.Code
		pause   bool
	}{
		{name: "kill succeeds", retCode: cubemasterclient.RetCodeSuccess, status: codes.Unset},
		{name: "kill rejected", retCode: cubemasterclient.RetCodeTaskStateInvalid, status: codes.Error},
		{name: "pause succeeds", pause: true, retCode: cubemasterclient.RetCodeSuccess, status: codes.Unset},
		{name: "pause rejected", pause: true, retCode: cubemasterclient.RetCodeMasterParamsError, status: codes.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := otel.GetTracerProvider()
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			otel.SetTracerProvider(provider)
			t.Cleanup(func() {
				otel.SetTracerProvider(previous)
				require.NoError(t, provider.Shutdown(context.Background()))
			})

			const sandboxID = "sbx-timeout"
			method, path := http.MethodDelete, "/cube/sandbox"
			actionSpan, clientSpan := telemetry.SpanSweepKill, telemetry.SpanCubeMasterKill
			if tc.pause {
				method, path = http.MethodPost, "/cube/sandbox/update"
				actionSpan, clientSpan = telemetry.SpanSweepPause, telemetry.SpanCubeMasterUpdate
			}
			headers := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, method, r.Method)
				assert.Equal(t, path, r.URL.Path)
				var req struct {
					SandboxID  string `json:"sandbox_id"`
					Sync       bool   `json:"sync"`
					KillReason string `json:"kill_reason"`
					Action     string `json:"action"`
				}
				if assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
					assert.Equal(t, sandboxID, req.SandboxID)
					if tc.pause {
						assert.Equal(t, "pause", req.Action)
					} else {
						assert.True(t, req.Sync)
						assert.Equal(t, cubemasterclient.KillReasonTimeout, req.KillReason)
					}
				}
				headers <- r.Header.Clone()
				_ = json.NewEncoder(w).Encode(map[string]any{"ret": map[string]int{"ret_code": tc.retCode}})
			}))
			defer server.Close()

			now := time.Now()
			reg := registry.New()
			seedEntry(t, reg, lifecycle.SandboxLifecycleMeta{
				SandboxID: sandboxID, InstanceType: "cubebox",
				TimeoutSeconds: lifecycle.TimeoutSecondsPtr(60), AutoPause: tc.pause,
			}, now.Add(-2*time.Minute).UnixMilli())
			s := newTestSweeper(reg, newFakeStore(), nil, newFakePush(), now)
			s.o.CubeMaster = cubemasterclient.New(server.URL, time.Second)

			// A standby does not generate a timeout trace or call CubeMaster.
			s.o.Leader = stubLeaderStatus(false)
			s.sweepOnce(context.Background())
			require.Empty(t, exporter.GetSpans())
			require.Empty(t, headers)

			s.o.Leader = stubLeaderStatus(true)
			s.sweepOnce(context.Background())

			spans := exporter.GetSpans()
			require.Len(t, spans, 2)
			byName := map[string]tracetest.SpanStub{}
			for _, span := range spans {
				byName[span.Name] = span
			}
			sweep, ok := byName[actionSpan]
			require.True(t, ok)
			client, ok := byName[clientSpan]
			require.True(t, ok)
			assert.False(t, sweep.Parent.IsValid())
			assert.Equal(t, trace.SpanKindClient, client.SpanKind)
			assert.Equal(t, sweep.SpanContext, client.Parent)
			assert.Equal(t, tc.status, sweep.Status.Code)
			assert.Equal(t, tc.status, client.Status.Code)
			assert.Contains(t, sweep.Attributes, attribute.String(telemetry.AttrSandboxID, sandboxID))
			assert.Contains(t, sweep.Attributes, attribute.Int64(telemetry.AttrIdleMS, 120_000))
			assert.Contains(t, sweep.Attributes, attribute.Int64(telemetry.AttrTimeoutMS, 60_000))
			assert.Contains(t, client.Attributes, attribute.Int(telemetry.AttrRetCode, tc.retCode))

			require.Len(t, headers, 1)
			incoming := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(<-headers))
			parent := trace.SpanContextFromContext(incoming)
			assert.Equal(t, client.SpanContext.TraceID(), parent.TraceID())
			assert.Equal(t, client.SpanContext.SpanID(), parent.SpanID())
			assert.True(t, parent.IsSampled())
		})
	}
}

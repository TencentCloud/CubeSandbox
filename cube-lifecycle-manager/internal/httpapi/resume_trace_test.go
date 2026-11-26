// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package httpapi

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
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/cubemasterclient"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/registry"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/resumer"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/telemetry"
)

func TestResumeContinuesProxyTrace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retCode    int
		httpStatus int
		spanStatus codes.Code
	}{
		{"success", cubemasterclient.RetCodeSuccess, http.StatusOK, codes.Unset},
		{"master rejects", cubemasterclient.RetCodeMasterParamsError, http.StatusServiceUnavailable, codes.Error},
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

			headers := make(chan http.Header, 1)
			master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/cube/sandbox/update", r.URL.Path)
				var body map[string]any
				if assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					assert.Equal(t, "resume", body["action"])
					assert.Equal(t, "sbx", body["sandbox_id"])
				}
				headers <- r.Header.Clone()
				_ = json.NewEncoder(w).Encode(map[string]any{"ret": map[string]int{"ret_code": tc.retCode}})
			}))
			defer master.Close()

			reg := registry.New()
			reg.Upsert(lifecycle.SandboxLifecycleMeta{
				SandboxID: "sbx", InstanceType: "cubebox", AutoResume: true,
			})
			r := resumer.New(resumer.Options{
				Registry:     reg,
				Redis:        newFakeStore(),
				CubeMaster:   cubemasterclient.New(master.URL, time.Second),
				ProxyPush:    fakePush{},
				StateLockTTL: time.Minute,
				Log:          zap.NewNop(),
			})
			server := New(":0", r, reg, zap.NewNop())
			req := httptest.NewRequest(http.MethodPost, "/internal/resume?sandbox_id=sbx&request_id=proxy-1", nil)
			req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			req.Header.Set("tracestate", "vendor=opaque")
			response := httptest.NewRecorder()
			server.handleResume(response, req)
			require.Equal(t, tc.httpStatus, response.Code)

			spans := exporter.GetSpans()
			require.Len(t, spans, 3)
			byName := map[string]tracetest.SpanStub{}
			for _, span := range spans {
				byName[span.Name] = span
			}
			proxy := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(req.Header))
			resume := byName[telemetry.SpanResume]
			ownership := byName[telemetry.SpanResumeOwnership]
			update := byName[telemetry.SpanCubeMasterUpdate]
			require.Equal(t, trace.SpanKindServer, resume.SpanKind)
			assert.Equal(t, trace.SpanContextFromContext(proxy), resume.Parent)
			assert.Equal(t, tc.spanStatus, resume.Status.Code)
			assert.Equal(t, resume.SpanContext, ownership.Parent)
			assert.Contains(t, ownership.Attributes, attribute.String(telemetry.AttrAction, "acquired"))
			assert.Equal(t, resume.SpanContext, update.Parent)
			assert.Equal(t, trace.SpanKindClient, update.SpanKind)
			assert.Equal(t, tc.spanStatus, update.Status.Code)
			assert.Contains(t, update.Attributes, attribute.Int(telemetry.AttrRetCode, tc.retCode))
			require.Len(t, headers, 1)
			out := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(<-headers))
			actual := trace.SpanContextFromContext(out)
			assert.Equal(t, update.SpanContext.TraceID(), actual.TraceID())
			assert.Equal(t, update.SpanContext.SpanID(), actual.SpanID())
			assert.Equal(t, "vendor=opaque", actual.TraceState().String())
		})
	}
}

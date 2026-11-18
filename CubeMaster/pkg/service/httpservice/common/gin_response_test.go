// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// These tests lock the response contract that the gin migration must preserve:
// every business response is HTTP 200 + Content-Type "application/json"
// (FastestJsoniter, NOT gin's default c.JSON which emits
// "application/json; charset=utf-8" via encoding/json), carrying the
// {ret:{ret_code,ret_msg}} envelope.

func newGin(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func decodeEnvelope(t *testing.T, body []byte) *types.Res {
	t.Helper()
	var r struct {
		Ret *types.Ret `json:"ret"`
	}
	requireNoErr(t, json.Unmarshal(body, &r))
	return &types.Res{Ret: r.Ret}
}

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWriteAPI_EnvelopeContract: success path is HTTP 200 + the JSON envelope
// via the FastestJsoniter write path (Content-Type exactly "application/json").
func TestWriteAPI_EnvelopeContract(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteAPI(c, &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: "ok"}})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// NOT gin's default "application/json; charset=utf-8" — proves the custom
	// FastestJsoniter write path is used, not c.JSON.
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	res := decodeEnvelope(t, w.Body.Bytes())
	if assert.NotNil(t, res.Ret) {
		assert.Equal(t, 0, res.Ret.RetCode)
		assert.Equal(t, "ok", res.Ret.RetMsg)
	}
}

func TestWriteAPI_AnnotatesBusinessOutcomeOnRouteSpan(t *testing.T) {
	previous := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(t.Context()) })

	tests := []struct {
		name, path string
		response   interface{}
		code       int
		status     codes.Code
	}{
		{"sandbox failure", "/cube/sandbox", &types.Res{Ret: &types.Ret{RetCode: 130001}}, 130001, codes.Error},
		{"template failure", "/cube/template/from-image", &types.CreateTemplateFromImageRes{Ret: &types.Ret{RetCode: 130001}}, 130001, codes.Error},
		{"panic recovery response", "/cube/sandbox", &types.Res{Ret: &types.Ret{RetCode: -1}}, -1, codes.Error},
		{"sandbox success", "/cube/sandbox", &types.Res{Ret: &types.Ret{RetCode: telemetry.SuccessCode}}, telemetry.SuccessCode, codes.Unset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exporter.Reset()
			r := newGin(t)
			r.Use(telemetry.GinMiddleware())
			r.POST(tc.path, func(c *gin.Context) { WriteAPI(c, tc.response) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, nil))
			assert.Equal(t, http.StatusOK, w.Code)
			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want one route span", len(spans))
			}
			assert.Equal(t, tc.status, spans[0].Status.Code)
			found := false
			for _, attr := range spans[0].Attributes {
				if string(attr.Key) == telemetry.AttrRetCode {
					found = true
					assert.Equal(t, int64(tc.code), attr.Value.AsInt64())
				}
			}
			assert.True(t, found, "route span lacks cube.ret_code")
		})
	}
}

// TestWriteErr_EnvelopeContract: the error helper emits 200 + a business error
// code/msg in the same envelope (HTTP status stays 200 by contract).
func TestWriteErr_EnvelopeContract(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteErr(c, 130001, "bad input")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	res := decodeEnvelope(t, w.Body.Bytes())
	if assert.NotNil(t, res.Ret) {
		assert.Equal(t, 130001, res.Ret.RetCode)
		assert.Equal(t, "bad input", res.Ret.RetMsg)
	}
}

// TestWriteListAPI_UsesListEncoder: the list path routes through
// WriteListResponse (bufferpool-backed), still HTTP 200 + application/json.
func TestWriteListAPI_UsesListEncoder(t *testing.T) {
	r := newGin(t)
	r.GET("/x", func(c *gin.Context) {
		WriteListAPI(c, &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: "ok"}})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	// list encoder writes the same envelope JSON
	decodeEnvelope(t, w.Body.Bytes())
}

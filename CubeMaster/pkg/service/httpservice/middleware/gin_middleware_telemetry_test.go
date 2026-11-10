// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package middleware_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	middleware "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/middleware"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestGinRequestMiddlewareRecoversPanicWithErrorSpan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLevel := CubeLog.GetLevel()
	CubeLog.SetLevel(CubeLog.INFO)
	defer CubeLog.SetLevel(previousLevel)
	patches := gomonkey.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Common: &config.CommonConf{}, AuthConf: &config.AuthConf{}}
	})
	defer patches.Reset()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	}()

	router := gin.New()
	router.Use(telemetry.GinMiddleware(), middleware.GinRequestMiddleware())
	reached := false
	var requestTrace *CubeLog.RequestTrace
	router.POST("/cube/sandbox", func(c *gin.Context) {
		reached = true
		requestTrace = CubeLog.GetTraceInfo(c.Request.Context())
		panic("recovery probe")
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/cube/sandbox", nil))
	if !reached {
		t.Fatal("request did not reach the panicking handler")
	}
	if requestTrace == nil || requestTrace.RetCode != -1 {
		t.Fatalf("request log ret_code = %v, want -1", requestTrace)
	}

	var body struct {
		Ret struct {
			RetCode int `json:"ret_code"`
		} `json:"ret"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode recovery response %q: %v", response.Body.String(), err)
	}
	if body.Ret.RetCode != -1 {
		t.Fatalf("recovery ret_code = %d, want -1; body=%s", body.Ret.RetCode, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("recovery HTTP status = %d, want %d", response.Code, http.StatusOK)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /cube/sandbox" || span.Status().Code != codes.Error {
		t.Fatalf("span name=%q status=%v, want route and ERROR", span.Name(), span.Status().Code)
	}
	for _, attr := range span.Attributes() {
		if string(attr.Key) == telemetry.AttrRetCode {
			if attr.Value.AsInt64() != -1 {
				t.Fatalf("cube.ret_code = %d, want -1", attr.Value.AsInt64())
			}
			return
		}
	}
	t.Fatal("entry span has no cube.ret_code")
}

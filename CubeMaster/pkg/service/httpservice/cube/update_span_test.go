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

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"go.opentelemetry.io/otel/codes"
)

const updateRoutePath = "POST /cube/sandbox/update"

// newUpdateRouter mirrors the server: request trace installed before GinMiddleware.
func newUpdateRouter() *gin.Engine {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(CubeLog.WithRequestTrace(c.Request.Context(), &CubeLog.RequestTrace{}))
		c.Next()
	})
	router.Use(telemetry.GinMiddleware())
	return router
}

func TestHandleUpdateActionMarksRouteSpanOutcome(t *testing.T) {
	const body = `{"requestID":"req-1","sandbox_id":"sb-1","instance_type":"cubebox","action":"pause"}`

	updateCalled := false
	updateStub := func(code int, msg string) func(*gomonkey.Patches) {
		return func(p *gomonkey.Patches) {
			p.ApplyFunc(sandbox.Update, func(context.Context, *types.UpdateRequest) *types.Res {
				updateCalled = true
				return &types.Res{Ret: &types.Ret{RetCode: code, RetMsg: msg}}
			})
		}
	}

	cases := []struct {
		name       string
		body       string
		stub       func(*gomonkey.Patches)
		wantCode   int
		wantStatus codes.Code
		wantCalled bool
	}{
		{
			name:       "malformed body",
			body:       "{",
			wantCode:   int(errorcode.ErrorCode_MasterParamsError),
			wantStatus: codes.Error,
		},
		{
			name:       "missing request id",
			body:       `{"sandbox_id":"sb-1"}`,
			wantCode:   int(errorcode.ErrorCode_MasterParamsError),
			wantStatus: codes.Error,
		},
		{
			name:       "business failure",
			body:       body,
			stub:       updateStub(int(errorcode.ErrorCode_MasterInternalError), "boom"),
			wantCode:   int(errorcode.ErrorCode_MasterInternalError),
			wantStatus: codes.Error,
			wantCalled: true,
		},
		{
			name:       "success",
			body:       body,
			stub:       updateStub(int(errorcode.ErrorCode_Success), "success"),
			wantCode:   int(errorcode.ErrorCode_Success),
			wantStatus: codes.Unset,
			wantCalled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, flush := setupSpanRecorder(t)
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			updateCalled = false
			if tc.stub != nil {
				tc.stub(patches)
			}

			router := newUpdateRouter()
			router.POST("/cube/sandbox/update", handleUpdateAction)

			req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/update", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			flush()

			if tc.wantCalled != updateCalled {
				t.Fatalf("sandbox.Update called = %v, want %v", updateCalled, tc.wantCalled)
			}
			spans := rec.named(updateRoutePath)
			require.Len(t, spans, 1)
			if spans[0].Status().Code != tc.wantStatus {
				t.Errorf("route span status = %v, want %v", spans[0].Status().Code, tc.wantStatus)
			}
			if got := attrInt(t, spans[0].Attributes(), telemetry.AttrRetCode); got != int64(tc.wantCode) {
				t.Errorf("route span %s = %d, want %d", telemetry.AttrRetCode, got, tc.wantCode)
			}
		})
	}
}

func TestHandleUpdateActionPanicIsMarkedAndPropagates(t *testing.T) {
	rec, flush := setupSpanRecorder(t)
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(sandbox.Update, func(context.Context, *types.UpdateRequest) *types.Res {
		panic("boom-update")
	})

	router := newUpdateRouter()
	var recovered any
	router.Use(func(c *gin.Context) {
		defer func() { recovered = recover() }()
		c.Next()
	})
	router.POST("/cube/sandbox/update", handleUpdateAction)

	req := httptest.NewRequest(http.MethodPost, "/cube/sandbox/update",
		strings.NewReader(`{"requestID":"req-1"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	flush()

	require.Equal(t, "boom-update", recovered)
	spans := rec.named(updateRoutePath)
	require.Len(t, spans, 1)
	if spans[0].Status().Code != codes.Error {
		t.Errorf("a panicking update must mark the route span error, got %v", spans[0].Status().Code)
	}
}

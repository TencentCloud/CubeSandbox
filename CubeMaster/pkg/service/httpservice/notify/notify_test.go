// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// newNotifyEngine wires the real /notify routes onto a gin engine exactly as
// production (RegisterNotifyRoutes) and injects a request trace the way the
// request middleware does — the handlers dereference the trace (rt.RetCode),
// so it must be non-nil. No DB / cluster is touched.
func newNotifyEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			CubeLog.WithRequestTrace(c.Request.Context(), &CubeLog.RequestTrace{}))
		c.Next()
	})
	RegisterNotifyRoutes(r.Group(NotifyURI()))
	return r
}

// retEnvelope decodes only the ret block so tests can assert on ret_code.
type retEnvelope struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
}

func decodeRetCode(t *testing.T, body []byte) int {
	t.Helper()
	var env retEnvelope
	require.NoError(t, common.FastestJsoniter.Unmarshal(body, &env))
	require.NotNil(t, env.Ret, "response must carry a ret envelope: %s", string(body))
	return env.Ret.RetCode
}

// TestHostChangeGoodBodyReturnsSuccessEnvelope proves a well-formed POST
// /notify/host body is parsed and forwarded to hostChangeNotify, which returns
// the success envelope (HTTP 200 / ret_code 200).
func TestHostChangeGoodBodyReturnsSuccessEnvelope(t *testing.T) {
	var gotReq *types.HostChangeEvent
	patch := gomonkey.ApplyFunc(hostChangeNotify,
		func(_ context.Context, req *types.HostChangeEvent) *types.Res {
			gotReq = req
			return &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_Success),
					RetMsg:  errorcode.ErrorCode_Success.String(),
				},
			}
		})
	defer patch.Reset()

	body := `{"requestID":"req-7","HostIDs":["host-a"],"EventType":"start"}`
	r := newNotifyEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, NotifyURI()+"/host", strings.NewReader(body)))

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, gotReq, "hostChangeNotify must be invoked with the parsed body")
	assert.Equal(t, "req-7", gotReq.RequestID)
	assert.Equal(t, []string{"host-a"}, gotReq.HostIDs)
	assert.Equal(t, "start", gotReq.EventType)
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// TestHostChangeBadBodyReturnsParamsErrorEnvelope locks the parse-time guard: a
// malformed body short-circuits to the MasterParamsError envelope (note: HTTP
// 200, because the handler uses WriteAPI, not a 4xx status) — mirroring the old
// mux behavior. No patch is needed since parsing fails before hostChangeNotify.
func TestHostChangeBadBodyReturnsParamsErrorEnvelope(t *testing.T) {
	r := newNotifyEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		NotifyURI()+"/host", strings.NewReader("{not json")))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), decodeRetCode(t, w.Body.Bytes()))
}

// TestHealthCheckReturnsSuccessEnvelope verifies GET /notify/health returns the
// success envelope without any DB dependency.
func TestHealthCheckReturnsSuccessEnvelope(t *testing.T) {
	r := newNotifyEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, NotifyURI()+"/health", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, int(errorcode.ErrorCode_Success), decodeRetCode(t, w.Body.Bytes()))
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

// TestFakeCreateReturnsLegacyEnvelope verifies H1: POST /internal/fake_create
// must preserve the legacy contract from the old mux inner.HttpHandler (which
// had no fake_create case → fell through to the default): HTTP 200 +
// {ret:{ret_code:-1, ret_msg:"Not Found"}}. The route must not silently 404.
func TestFakeCreateReturnsLegacyEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// inject a request trace the way the production middleware does, so the
	// handler runs exactly as in production (GetTraceInfo is non-nil).
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(CubeLog.WithRequestTrace(c.Request.Context(), &CubeLog.RequestTrace{}))
		c.Next()
	})
	r.POST(FakeCreateAction, fakeCreateGinHandler)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, FakeCreateAction, nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	body := w.Body.String()
	assert.Contains(t, body, `"ret_code":-1`)
	assert.Contains(t, body, "Not Found")
}

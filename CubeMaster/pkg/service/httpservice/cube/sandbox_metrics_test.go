// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestHandleSandboxMetricsActionRejectsInvalidStartQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	rt := &CubeLog.RequestTrace{}
	req := httptest.NewRequest(http.MethodGet, "/cube/sandbox/metrics?sandbox_id=sb-1&start=bad", nil)
	req = req.WithContext(CubeLog.WithRequestTrace(req.Context(), rt))
	c.Request = req

	handleSandboxMetricsAction(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ret_code":`+strconv.Itoa(int(errorcode.ErrorCode_MasterParamsError)))
	assert.Contains(t, w.Body.String(), "invalid syntax")
	assert.Equal(t, int64(errorcode.ErrorCode_MasterParamsError), rt.RetCode)
	assert.Contains(t, w.Body.String(), `"data":[]`)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// A redo body without request_id leaves the embedded *Request nil; the
// handler must reject it with a params error instead of nil-dereferencing.
func TestHandleRedoTemplateActionNilRequestNoPanic(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	trace := &CubeLog.RequestTrace{}
	ctx.Request = httptest.NewRequest("POST", "/cube/template/redo",
		strings.NewReader(`{"template_id":"tpl-1"}`)).
		WithContext(CubeLog.WithRequestTrace(context.Background(), trace))

	handleRedoTemplateAction(ctx)

	var got types.CreateTemplateFromImageRes
	if err := common.FastestJsoniter.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("response must be a CreateTemplateFromImageRes even for rejected bodies: %v (body=%q)", err, recorder.Body.String())
	}
	if got.Ret == nil || got.Ret.RetCode != int(errorcode.ErrorCode_MasterParamsError) {
		t.Fatalf("RetCode = %+v, want MasterParamsError for a body without request_id", got.Ret)
	}
}

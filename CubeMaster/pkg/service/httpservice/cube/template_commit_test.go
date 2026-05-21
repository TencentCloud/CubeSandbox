// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func TestHandleSandboxCommitActionRejectsEmptyRequestID(t *testing.T) {
	body := `{
		"sandbox_id":"sb-1",
		"template_id":"tpl-1",
		"create_request":{
			"instance_type":"cubebox",
			"network_type":"tap",
			"annotations":{
				"cube.master.appsnapshot.template.id":"tpl-1",
				"cube.master.appsnapshot.template.version":"v2"
			}
		}
	}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	resp := handleSandboxCommitAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*commitTemplateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	if got.Res == nil || got.Res.Ret == nil {
		t.Fatalf("missing Ret in response: %#v", got)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Res.Ret.RetCode)
	assert.Contains(t, got.Res.Ret.RetMsg, "requestID is required")
	assert.Equal(t, "tpl-1", got.TemplateID)
}

func TestHandleSandboxCommitActionRejectsMissingFields(t *testing.T) {
	body := `{"requestID":"req-1"}`
	req := httptest.NewRequest("POST", "/cube/sandbox/commit", strings.NewReader(body))
	rt := &CubeLog.RequestTrace{}
	resp := handleSandboxCommitAction(httptest.NewRecorder(), req, rt)

	got, ok := resp.(*commitTemplateResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", resp)
	}
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), got.Res.Ret.RetCode)
	assert.Contains(t, got.Res.Ret.RetMsg, "sandbox_id, template_id and create_request are required")
}

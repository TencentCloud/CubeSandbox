// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"testing"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// TestCloneAgent_CompensationCorrelatesRequestID verifies that when
// CloneAgent's applyOpenclawRuntime fails, the compensation DeleteSandbox body
// reuses the inbound RequestTrace.RequestID so CubeMaster logs correlate.
func TestCloneAgent_CompensationCorrelatesRequestID(t *testing.T) {
	const inboundReqID = "clone-inbound-req-123"
	ctx := cubelog.WithRequestTrace(context.Background(), &cubelog.RequestTrace{RequestID: inboundReqID})

	var capturedBody map[string]interface{}
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}

	h := &AgentHubHandler{cm: cm}
	h.compensateDeleteSandbox(ctx, "sb-clone-test", "clone_upsert")

	if capturedBody == nil {
		t.Fatal("DeleteSandbox was not called")
	}
	for _, key := range []string{"requestID", "RequestID", "request_id"} {
		if got := capturedBody[key]; got != inboundReqID {
			t.Errorf("%s = %v, want %q", key, got, inboundReqID)
		}
	}
	if capturedBody["sandbox_id"] != "sb-clone-test" {
		t.Errorf("sandbox_id = %v, want %q", capturedBody["sandbox_id"], "sb-clone-test")
	}
	if capturedBody["instance_type"] != "cubebox" {
		t.Errorf("instance_type = %v, want %q", capturedBody["instance_type"], "cubebox")
	}
}

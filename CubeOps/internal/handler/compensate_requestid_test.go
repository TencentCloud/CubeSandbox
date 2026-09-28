// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"testing"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// TestCompensateDeleteSandbox_CorrelatesRequestID verifies that the
// compensation DeleteSandbox body carries the inbound RequestTrace.RequestID
// so CubeMaster stat logs can be joined with the CubeOps request.
func TestCompensateDeleteSandbox_CorrelatesRequestID(t *testing.T) {
	const inboundReqID = "inbound-req-123"
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
	h.compensateDeleteSandbox(ctx, "sb-test", "unit-test")

	if capturedBody == nil {
		t.Fatal("DeleteSandbox was not called")
	}
	for _, key := range []string{"requestID", "RequestID", "request_id"} {
		if got := capturedBody[key]; got != inboundReqID {
			t.Errorf("%s = %v, want %q", key, got, inboundReqID)
		}
	}
}

// TestCompensateDeleteSandbox_HasSandboxIDAndInstanceType verifies the other
// required fields are present in the DeleteSandbox body.
func TestCompensateDeleteSandbox_HasSandboxIDAndInstanceType(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}

	h := &AgentHubHandler{cm: cm}
	h.compensateDeleteSandbox(context.Background(), "sb-123", "test")

	if capturedBody["sandbox_id"] != "sb-123" {
		t.Errorf("sandbox_id = %v, want %q", capturedBody["sandbox_id"], "sb-123")
	}
	if capturedBody["instance_type"] != "cubebox" {
		t.Errorf("instance_type = %v, want %q", capturedBody["instance_type"], "cubebox")
	}
}

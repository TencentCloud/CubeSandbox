// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestS3_DeleteSandboxRequestBody_UsesRequestID verifies that the request
// body sent to CubeMaster DeleteSandbox uses the JSON key "requestID" (not
// "request_id" or "RequestID"), matching DeleteCubeSandboxReq's json tag.
//
// This is a contract test: it captures the actual map sent by the handler
// and asserts the field name. The existing fault-injection tests
// (agenthub_fault_injection_test.go) also assert this, but they require
// Docker (dockertest). This test runs without Docker by invoking the
// compensation path directly with a fake CM.
//
// See review S3.
func TestS3_DeleteSandboxRequestBody_UsesRequestID(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}

	// compensateDeleteSandbox sends a DeleteSandbox request with a
	// "requestID" key. We call it directly — it only needs cm, not store.
	h := &AgentHubHandler{cm: cm}
	h.compensateDeleteSandbox(context.Background(), "sb-test", "unit-test")

	if capturedBody == nil {
		t.Fatal("DeleteSandbox was not called")
	}

	// S3 fix: must use "requestID", NOT "request_id" or "RequestID".
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("requestID = %v, want non-empty string; full body = %v",
			capturedBody["requestID"], capturedBody)
	}
	if _, exists := capturedBody["request_id"]; exists {
		t.Error(`body contains "request_id" — must be "requestID" (S3)`)
	}
	if _, exists := capturedBody["RequestID"]; exists {
		t.Error(`body contains "RequestID" — must be "requestID" (S3)`)
	}

	// Verify the prefix matches the compensation convention.
	if !strings.HasPrefix(reqID, "cubeops-compensate-") {
		t.Errorf("requestID = %q, want prefix %q", reqID, "cubeops-compensate-")
	}
}

// TestS3_DeleteSandboxRequestBody_HasSandboxIDAndInstanceType verifies the
// other required fields are present in the DeleteSandbox body.
func TestS3_DeleteSandboxRequestBody_HasSandboxIDAndInstanceType(t *testing.T) {
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

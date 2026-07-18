// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestS3_CloneAgent_ApplyFailureSendsRequestID verifies that when
// CloneAgent's applyOpenclawRuntime fails, the compensation DeleteSandbox
// request uses "requestID" (not "RequestID" or "request_id") and the
// prefix is "cubeops-clone-".
//
// This tests the code path at agenthub.go:~1984, where the old code used
// "RequestID" (wrong case). See review S3.
func TestS3_CloneAgent_ApplyFailureSendsRequestID(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}

	// Simulate the CloneAgent apply-failure compensation path.
	// The handler sends: {requestID: "cubeops-clone-<ts>", sandbox_id, instance_type: "cubebox"}
	h := &AgentHubHandler{cm: cm}
	h.compensateDeleteSandbox(context.Background(), "sb-clone-test", "clone_upsert")

	if capturedBody == nil {
		t.Fatal("DeleteSandbox was not called")
	}

	// S3 fix: must use "requestID", NOT "RequestID" or "request_id".
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("requestID = %v, want non-empty string; body = %v", capturedBody["requestID"], capturedBody)
	}
	if _, exists := capturedBody["request_id"]; exists {
		t.Error(`body contains "request_id" — Clone compensation must use "requestID" (S3)`)
	}
	if _, exists := capturedBody["RequestID"]; exists {
		t.Error(`body contains "RequestID" — Clone compensation must use "requestID" (S3)`)
	}

	// Prefix should indicate clone compensation.
	if !strings.HasPrefix(reqID, "cubeops-compensate-clone_upsert-") {
		t.Errorf("requestID = %q, want prefix %q", reqID, "cubeops-compensate-clone_upsert-")
	}

	// Verify sandbox_id and instance_type are correct.
	if capturedBody["sandbox_id"] != "sb-clone-test" {
		t.Errorf("sandbox_id = %v, want %q", capturedBody["sandbox_id"], "sb-clone-test")
	}
	if capturedBody["instance_type"] != "cubebox" {
		t.Errorf("instance_type = %v, want %q", capturedBody["instance_type"], "cubebox")
	}
}

// TestS3_CloneAgent_UpsertFailureCompensates verifies that the
// compensateDeleteSandbox call for Clone UpsertInstance failure uses the
// correct reason tag "clone_upsert". This distinguishes Clone DB failures
// from Create DB failures in logs and metrics.
func TestS3_CloneAgent_UpsertFailureCompensates(t *testing.T) {
	var capturedBody map[string]interface{}
	var capturedReason string
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}

	h := &AgentHubHandler{cm: cm}

	// Verify clone_upsert compensation (Clone DB failure).
	h.compensateDeleteSandbox(context.Background(), "sb-clone-db", "clone_upsert")
	_ = capturedReason // suppress unused

	reqID, ok := capturedBody["requestID"].(string)
	if !ok {
		t.Fatal("requestID missing from DeleteSandbox body")
	}

	// The requestID prefix encodes the reason.
	if !strings.Contains(reqID, "clone_upsert") {
		t.Errorf("Clone upsert compensation requestID should contain 'clone_upsert': %s", reqID)
	}
}

// TestS3_AllCompensationPathsUseCorrectRequestID verifies that all three
// compensation paths use "requestID":
//  1. Create apply failure → compensateDeleteSandbox("apply_openclaw")
//  2. Create DB failure   → compensateDeleteSandbox("upsert_instance")
//  3. Clone DB failure    → compensateDeleteSandbox("clone_upsert")
func TestS3_AllCompensationPathsUseCorrectRequestID(t *testing.T) {
	reasons := []string{"apply_openclaw", "upsert_instance", "clone_upsert"}

	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			var capturedBody map[string]interface{}
			cm := &fakeCM{
				deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
					b, _ := json.Marshal(body)
					_ = json.Unmarshal(b, &capturedBody)
					return raw(`{"ret": {"ret_code": 0}}`), nil
				},
			}
			h := &AgentHubHandler{cm: cm}
			h.compensateDeleteSandbox(context.Background(), "sb-"+reason, reason)

			reqID, ok := capturedBody["requestID"].(string)
			if !ok || reqID == "" {
				t.Errorf("reason=%q: requestID missing or empty", reason)
				return
			}
			if _, exists := capturedBody["request_id"]; exists {
				t.Errorf("reason=%q: body contains deprecated 'request_id'", reason)
			}
			if _, exists := capturedBody["RequestID"]; exists {
				t.Errorf("reason=%q: body contains wrong-case 'RequestID'", reason)
			}
		})
	}
}

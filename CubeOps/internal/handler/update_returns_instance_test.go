// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"os"
	"strings"
	"testing"
)

// TestS5_UpdateModelReturnsInstanceNot204 verifies that UpdateModel returns
// the updated instance (HTTP 200 with JSON body), not 204 No Content.
//
// Before the S5 fix, UpdateModel called httputil.WriteNoContent(c), returning
// 204. The frontend declared the return type as AgentInstanceDto and tried to
// read agent.id, causing a TypeError on the undefined response.
//
// This is a source-level contract test: it reads the handler source and
// asserts UpdateModel/UpdateWecomConfig use WriteJSON (not WriteNoContent)
// in their success paths. It does not need Docker.
//
// See review S5.
func TestS5_UpdateModelReturnsInstanceNot204(t *testing.T) {
	src, err := os.ReadFile("agenthub.go")
	if err != nil {
		t.Fatalf("read agenthub.go: %v", err)
	}
	srcStr := string(src)

	// Extract the UpdateModel function body.
	start := strings.Index(srcStr, "func (h *AgentHubHandler) UpdateModel(")
	if start < 0 {
		t.Fatal("UpdateModel function not found")
	}
	// Find the next function at the same indentation level (func ...).
	nextFunc := strings.Index(srcStr[start+1:], "\nfunc ")
	if nextFunc < 0 {
		t.Fatal("could not find end of UpdateModel function")
	}
	updateModelBody := srcStr[start : start+1+nextFunc]

	// S5 fix: success path must NOT use WriteNoContent.
	// The old code had: httputil.WriteNoContent(c)
	// The new code has: httputil.WriteJSON(c, http.StatusOK, updated)
	if strings.Contains(updateModelBody, "WriteNoContent(c)") {
		t.Error("UpdateModel still calls WriteNoContent in its success path — must return the updated instance (S5)")
	}
	if !strings.Contains(updateModelBody, "WriteJSON") {
		t.Error("UpdateModel does not call WriteJSON — must return the updated instance (S5)")
	}
}

// TestS5_UpdateWecomReturnsInstanceNot204 is the symmetric check for
// UpdateWecomConfig.
func TestS5_UpdateWecomReturnsInstanceNot204(t *testing.T) {
	src, err := os.ReadFile("agenthub.go")
	if err != nil {
		t.Fatalf("read agenthub.go: %v", err)
	}
	srcStr := string(src)

	// Extract the UpdateWecomConfig function body.
	start := strings.Index(srcStr, "func (h *AgentHubHandler) UpdateWecomConfig(")
	if start < 0 {
		t.Fatal("UpdateWecomConfig function not found")
	}
	nextFunc := strings.Index(srcStr[start+1:], "\nfunc ")
	if nextFunc < 0 {
		t.Fatal("could not find end of UpdateWecomConfig function")
	}
	body := srcStr[start : start+1+nextFunc]

	// The success path (after persisting to DB) must NOT use WriteNoContent.
	// Note: there may be error-path WriteNoContent calls elsewhere, but the
	// success path is the last write before return.
	if strings.Contains(body, "WriteNoContent(c)") {
		t.Error("UpdateWecomConfig still calls WriteNoContent in its success path — must return the updated instance (S5)")
	}
	if !strings.Contains(body, "WriteJSON") {
		t.Error("UpdateWecomConfig does not call WriteJSON — must return the updated instance (S5)")
	}
}

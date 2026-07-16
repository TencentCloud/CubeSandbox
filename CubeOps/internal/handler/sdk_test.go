// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// ── ListSandboxes ─────────────────────────────────────────────────────────

func TestSDK_ListSandboxes_Success(t *testing.T) {
	cm := &fakeCM{
		listSandboxesWithBody: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			// CubeMaster returns {ret, data: [items]}
			return raw(`{
				"ret": {"ret_code": 0, "ret_msg": "ok"},
				"data": [
					{"sandbox_id": "sb-1", "host_id": "node-a", "cpu_count": 2, "memory_mb": 4096,
					 "create_at": 1700000000000000000, "template_id": "tpl-1",
					 "annotations": {}, "labels": {"owner": "alice"}}
				]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "GET", "/api/v1/sdk/sandboxes", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// The response is a JSON array, not an object — decode it manually.
	// doJSON decodes into a map which fails for arrays; re-fetch the body.
	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes")
	var items []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal array: %v body=%s", err, w.Body.String())
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	// cpuCount is converted to millicores string "2000m".
	if items[0]["cpuCount"] != "2000m" {
		t.Errorf("cpuCount = %v, want 2000m", items[0]["cpuCount"])
	}
	if items[0]["sandboxID"] != "sb-1" {
		t.Errorf("sandboxID = %v, want sb-1", items[0]["sandboxID"])
	}
	if items[0]["memoryMB"] != float64(4096) {
		t.Errorf("memoryMB = %v, want 4096", items[0]["memoryMB"])
	}
	// Labels should be promoted to "metadata".
	if meta, ok := items[0]["metadata"].(map[string]interface{}); !ok || meta["owner"] != "alice" {
		t.Errorf("metadata = %v, want {owner:alice}", items[0]["metadata"])
	}
}

func TestSDK_ListSandboxes_CMError(t *testing.T) {
	cm := &fakeCM{
		listSandboxesWithBody: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return nil, errors.New("cubemaster unreachable")
		},
	}
	r := newSDKRouter(t, cm)

	code, body := doJSON(t, r, "GET", "/api/v1/sdk/sandboxes", "")
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", code)
	}
	if body["error"] == nil || !contains(body["error"].(string), "unreachable") {
		t.Errorf("error = %v, want contains 'unreachable'", body["error"])
	}
}

// ── GetSandbox ────────────────────────────────────────────────────────────

func TestSDK_GetSandbox_Success(t *testing.T) {
	cm := &fakeCM{
		getSandbox: func(_ context.Context, sandboxID, instanceType string) (json.RawMessage, error) {
			if sandboxID != "sb-42" {
				t.Errorf("sandboxID = %q, want sb-42", sandboxID)
			}
			return raw(`{
				"ret": {"ret_code": 0},
				"data": [{
					"sandbox_id": "sb-42", "host_id": "node-a", "status": 1,
					"template_id": "tpl-1", "namespace": "default",
					"containers": [{"container_id": "c-1", "cpu": "2000m", "mem": "2048Mi", "create_at": 1700000000000000000, "type": "sandbox"}],
					"annotations": {}, "labels": {}
				}]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes/sb-42")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	// cpuCount is passed through as-is ("2000m") from container spec.
	if detail["cpuCount"] != "2000m" {
		t.Errorf("cpuCount = %v, want 2000m", detail["cpuCount"])
	}
	if detail["memoryMB"] != float64(2048) {
		t.Errorf("memoryMB = %v, want 2048", detail["memoryMB"])
	}
	if detail["state"] != "running" {
		t.Errorf("state = %v, want running (status 1 → running)", detail["state"])
	}
}

func TestSDK_GetSandbox_NotFoundInCM(t *testing.T) {
	cm := &fakeCM{
		getSandbox: func(_ context.Context, _, _ string) (json.RawMessage, error) {
			// CubeMaster returns ret_code 130404 → handler must map to HTTP 404
			// (review-bot flag: the previous version returned 200 + null body,
			// which violated REST conventions and the existing-test was
			// asserting the buggy "existing behavior" rather than the fix).
			return raw(`{"ret": {"ret_code": 130404, "ret_msg": "sandbox not found"}, "data": []}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes/nope")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (ret_code 130404 must map to NotFound); body=%s", w.Code, w.Body.String())
	}
	// Body should carry the CubeMaster ret_msg so the client sees the cause.
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v body=%s", err, w.Body.String())
	}
	if got, _ := body["error"].(string); got != "sandbox not found" {
		t.Errorf("error = %q, want %q (CubeMaster ret_msg must surface in the response)", got, "sandbox not found")
	}
}

// ── DeleteSandbox ─────────────────────────────────────────────────────────

func TestSDK_DeleteSandbox_Success(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "request_id": "r-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/sb-99")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Verify the handler passed sandbox_id from URL path into the CM body.
	if capturedBody["sandbox_id"] != "sb-99" {
		t.Errorf("CM body sandbox_id = %v, want sb-99", capturedBody["sandbox_id"])
	}
	if capturedBody["instance_type"] != "cubebox" {
		t.Errorf("CM body instance_type = %v, want cubebox", capturedBody["instance_type"])
	}
}

// ── PauseSandbox / ResumeSandbox ──────────────────────────────────────────

func TestSDK_PauseSandbox_PassesAction(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		updateSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "POST", "/api/v1/sdk/sandboxes/sb-1/pause", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if capturedBody["action"] != "pause" {
		t.Errorf("action = %v, want pause", capturedBody["action"])
	}
	if capturedBody["sandbox_id"] != "sb-1" {
		t.Errorf("sandbox_id = %v, want sb-1", capturedBody["sandbox_id"])
	}
}

func TestSDK_ResumeSandbox_PassesAction(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		updateSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "POST", "/api/v1/sdk/sandboxes/sb-1/resume", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if capturedBody["action"] != "resume" {
		t.Errorf("action = %v, want resume", capturedBody["action"])
	}
}

// ── CreateSandbox validation ──────────────────────────────────────────────

func TestSDK_CreateSandbox_MissingTemplateID(t *testing.T) {
	cm := &fakeCM{} // createSandbox nil — handler must reject before calling CM
	r := newSDKRouter(t, cm)

	code, body := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if body["error"] == nil {
		t.Errorf("error missing")
	}
}

func TestSDK_CreateSandbox_Success(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "new-sb", "host_id": "node-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes", `{"templateID": "tpl-x"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if capturedBody["template_id"] != "tpl-x" {
		t.Errorf("template_id = %v, want tpl-x", capturedBody["template_id"])
	}
}

// ── CM ret_code mapping ───────────────────────────────────────────────────

func TestSDK_CMRetCode404_MapsToHTTP404(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130404, "ret_msg": "not found"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/missing")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (ret_code 130404); body=%s", w.Code, w.Body.String())
	}
}

func TestSDK_CMRetCode409_MapsToHTTP409(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130409, "ret_msg": "conflict"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/x")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (ret_code 130409); body=%s", w.Code, w.Body.String())
	}
}

// ── Templates ─────────────────────────────────────────────────────────────

func TestSDK_GetTemplate_Success(t *testing.T) {
	cm := &fakeCM{
		listTemplates: func(_ context.Context, templateID string, includeReq bool) (json.RawMessage, error) {
			if templateID != "tpl-7" {
				t.Errorf("templateID = %q, want tpl-7", templateID)
			}
			if !includeReq {
				t.Errorf("includeReq = false, want true (GetTemplate always passes true)")
			}
			return raw(`{
				"ret": {"ret_code": 0},
				"template_id": "tpl-7", "image_info": "ubuntu:22.04",
				"create_request": {"network_type": "tap"}
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/templates/tpl-7")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var tpl map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &tpl); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	// snake_case → camelCase conversion.
	if tpl["templateID"] != "tpl-7" {
		t.Errorf("templateID = %v, want tpl-7", tpl["templateID"])
	}
	// networkType is promoted from createRequest to top level.
	if tpl["networkType"] != "tap" {
		t.Errorf("networkType = %v, want tap (promoted from createRequest)", tpl["networkType"])
	}
}

func TestSDK_GetTemplate_NotFound(t *testing.T) {
	cm := &fakeCM{
		listTemplates: func(_ context.Context, _ string, _ bool) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130404, "ret_msg": "not found"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/templates/missing")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSDK_CreateTemplate_MissingImage(t *testing.T) {
	cm := &fakeCM{}
	r := newSDKRouter(t, cm)

	code, body := doJSON(t, r, "POST", "/api/v1/sdk/templates", `{"templateID": "x"}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (image required)", code)
	}
	if body["error"] == nil {
		t.Error("error missing")
	}
}

// ── GetTemplateCompat — literal path vs :id ──────────────────────────────

func TestSDK_GetTemplateCompat_LiteralPath(t *testing.T) {
	// This test guards against regression: "compat" must NOT be captured by
	// the :id route. If it is, GetTemplate would be called with "compat"
	// instead of GetTemplateCompat.
	var calledCompat bool
	cm := &fakeCM{
		getTemplateCompat: func(_ context.Context) (json.RawMessage, error) {
			calledCompat = true
			return raw(`{"ret": {"ret_code": 0}, "data": {"compat": true}}`), nil
		},
		// If listTemplates is hit, the test fails:
		listTemplates: func(_ context.Context, id string, _ bool) (json.RawMessage, error) {
			t.Errorf("listTemplates called with id=%q — 'compat' was matched by :id route instead of literal path", id)
			return nil, errors.New("should not be called")
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/templates/compat")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !calledCompat {
		t.Error("GetTemplateCompat was not called")
	}
}

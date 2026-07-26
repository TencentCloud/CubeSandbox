// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// ── R13: CreateSandbox request/response model ─────────────────────────────

// TestR13_CreateSandbox_ForwardsAutoPause verifies that autoPause=true from
// the WebUI is forwarded to CubeMaster as auto_pause=true (not hardcoded
// false). See R13.
func TestR13_CreateSandbox_ForwardsAutoPause(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x", "autoPause": true}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if capturedBody["auto_pause"] != true {
		t.Errorf("auto_pause = %v, want true (R13: forward autoPause from WebUI)", capturedBody["auto_pause"])
	}
}

// TestR13_CreateSandbox_AutoPauseDefaultsFalse verifies that when autoPause
// is absent, auto_pause defaults to false (not lost or nil).
func TestR13_CreateSandbox_AutoPauseDefaultsFalse(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if capturedBody["auto_pause"] != false {
		t.Errorf("auto_pause = %v, want false (default)", capturedBody["auto_pause"])
	}
}

// TestR13_CreateSandbox_ForwardsAliasAsLabel verifies that the alias field
// from the WebUI is forwarded as a label so CubeMaster/Cubelet can display it.
// See R13.
func TestR13_CreateSandbox_ForwardsAliasAsLabel(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x", "alias": "my-sandbox"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	labels, ok := capturedBody["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("labels = %T, want map", capturedBody["labels"])
	}
	if labels["alias"] != "my-sandbox" {
		t.Errorf("labels[alias] = %v, want \"my-sandbox\" (R13: forward alias as label)", labels["alias"])
	}
}

// TestR13_CreateSandbox_TimeoutForwarded verifies that a positive timeout
// from the WebUI is forwarded to CubeMaster as an int. See R13.
func TestR13_CreateSandbox_TimeoutForwarded(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x", "timeout": 3600}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if capturedBody["timeout"] != float64(3600) {
		t.Errorf("timeout = %v, want 3600 (R13: forward positive timeout)", capturedBody["timeout"])
	}
}

// TestR13_CreateSandbox_TimeoutZeroOmitted verifies that when timeout=0 or
// is absent, the timeout field is NOT sent to CubeMaster (so CubeMaster
// applies its own default). The old code hardcoded 86400. See R13.
func TestR13_CreateSandbox_TimeoutZeroOmitted(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	// timeout=0 explicitly
	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x", "timeout": 0}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, exists := capturedBody["timeout"]; exists {
		t.Errorf("timeout = %v, want absent (R13: timeout=0 means use CubeMaster default, not 86400)", capturedBody["timeout"])
	}
}

// TestR13_CreateSandbox_TimeoutAbsentOmitted verifies that when timeout is
// not in the request at all, the timeout field is NOT sent to CubeMaster.
func TestR13_CreateSandbox_TimeoutAbsentOmitted(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if _, exists := capturedBody["timeout"]; exists {
		t.Errorf("timeout = %v, want absent (R13: no timeout → use CubeMaster default)", capturedBody["timeout"])
	}
}

// TestR13_CreateSandbox_MetadataAsLabels verifies that the metadata map from
// the WebUI is converted to labels (string→string). See R13.
func TestR13_CreateSandbox_MetadataAsLabels(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x", "metadata": {"env": "staging", "team": "qa"}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	labels, ok := capturedBody["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("labels = %T, want map", capturedBody["labels"])
	}
	if labels["env"] != "staging" {
		t.Errorf("labels[env] = %v, want \"staging\"", labels["env"])
	}
	if labels["team"] != "qa" {
		t.Errorf("labels[team] = %v, want \"qa\"", labels["team"])
	}
}

// TestR13_CreateSandbox_RequiredFields verifies that the CM request always
// includes the required lifecycle/network/env fields that were lost in the
// regression. See R13.
func TestR13_CreateSandbox_RequiredFields(t *testing.T) {
	var capturedBody map[string]interface{}
	cm := &fakeCM{
		createSandbox: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			b, _ := json.Marshal(body)
			_ = json.Unmarshal(b, &capturedBody)
			return raw(`{"ret": {"ret_code": 0}, "sandbox_id": "sb-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	code, _ := doJSON(t, r, "POST", "/api/v1/sdk/sandboxes",
		`{"templateID": "tpl-x"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	// Required fields that must always be present (R13: were lost in regression).
	required := []string{"requestID", "instance_type", "template_id", "annotations",
		"labels", "volumes", "containers", "exposed_ports", "network_type",
		"auto_pause", "auto_resume"}
	for _, field := range required {
		if _, exists := capturedBody[field]; !exists {
			t.Errorf("required field %q missing from CM request (R13: lifecycle/network/env fields must not be lost)", field)
		}
	}

	// network_type must be "tap" (R13: was lost in regression).
	if capturedBody["network_type"] != "tap" {
		t.Errorf("network_type = %v, want \"tap\"", capturedBody["network_type"])
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

// TestSDK_CMRetCodePausing_MapsToHTTP503 verifies that ret_code 130490
// (sandbox is pausing) maps to 503 + Retry-After header, NOT 502. See R11.
func TestSDK_CMRetCodePausing_MapsToHTTP503(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130490, "ret_msg": "sandbox is pausing; retry later"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/pausing")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (ret_code 130490); body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After = %q, want \"2\"", ra)
	}
}

// TestSDK_CMRetCodeResumeFailed_MapsToHTTP503 verifies that ret_code 130589
// (resume failed) maps to 503 + Retry-After=5, NOT 502. See R11.
func TestSDK_CMRetCodeResumeFailed_MapsToHTTP503(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130589, "ret_msg": "resume timeout"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/resume-fail")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (ret_code 130589); body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want \"5\"", ra)
	}
}

// TestSDK_CMRetCodeUnknown_MapsToHTTP502 verifies that an unrecognized
// ret_code still falls through to 502 (the default). See R11.
func TestSDK_CMRetCodeUnknown_MapsToHTTP502(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130500, "ret_msg": "internal error"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/unknown")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (ret_code 130500); body=%s", w.Code, w.Body.String())
	}
}

// TestSDK_NetworkError_MapsToHTTP502 verifies that a non-CMError (network
// failure) maps to 502, proving the default path still works. See R11.
func TestSDK_NetworkError_MapsToHTTP502(t *testing.T) {
	cm := &fakeCM{
		deleteSandbox: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return nil, errors.New("connection refused")
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/sandboxes/neterr")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (network error); body=%s", w.Code, w.Body.String())
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

// ── AdoptTemplateCompatBaseline — POST adopt-baseline ───────────────────

func TestSDK_AdoptTemplateCompatBaseline(t *testing.T) {
	var receivedBody interface{}
	cm := &fakeCM{
		adoptTemplateCompatBaseline: func(_ context.Context, body interface{}) (json.RawMessage, error) {
			receivedBody = body
			return raw(`{"ret": {"ret_code": 0}, "data": {"updated": 3}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "POST", "/api/v1/sdk/templates/compat/tpl-abc/adopt-baseline")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Verify the CubeMaster request body has the right action and template_id.
	bodyMap, ok := receivedBody.(map[string]interface{})
	if !ok {
		t.Fatalf("received body is not a map: %T", receivedBody)
	}
	if bodyMap["action"] != "adopt_baseline" {
		t.Errorf("action = %v, want adopt_baseline", bodyMap["action"])
	}
	if bodyMap["template_id"] != "tpl-abc" {
		t.Errorf("template_id = %v, want tpl-abc", bodyMap["template_id"])
	}

	// Verify the response contains the updated count.
	if !strings.Contains(w.Body.String(), `"updated"`) {
		t.Errorf("response body does not contain 'updated': %s", w.Body.String())
	}
}

// ── Route completeness — all WebUI SDK paths must be registered ─────────

func TestSDK_RouteCompleteness(t *testing.T) {
	// Every path the WebUI client.ts calls must map to a registered route.
	// If a route is missing the handler returns 404, which is exactly the
	// R04 bug class (adopt-baseline was missing). This test enumerates the
	// WebUI SDK surface so future route migrations don't silently drop one.
	cm := &fakeCM{}
	r := newSDKRouter(t, cm)

	// Each entry: {method, path}. Paths taken from web/src/api/client.ts.
	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/sdk/sandboxes"},
		{"POST", "/api/v1/sdk/sandboxes"},
		{"GET", "/api/v1/sdk/sandboxes/sb-1"},
		{"DELETE", "/api/v1/sdk/sandboxes/sb-1"},
		{"GET", "/api/v1/sdk/sandboxes/sb-1/logs"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/timeout"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/refreshes"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/pause"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/resume"},
		{"GET", "/api/v1/sdk/snapshots"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/snapshots"},
		{"POST", "/api/v1/sdk/sandboxes/sb-1/rollback"},
		{"GET", "/api/v1/sdk/templates"},
		{"POST", "/api/v1/sdk/templates"},
		{"GET", "/api/v1/sdk/templates/compat"},
		{"GET", "/api/v1/sdk/templates/tpl-1"},
		{"POST", "/api/v1/sdk/templates/tpl-1"},
		{"DELETE", "/api/v1/sdk/templates/tpl-1"},
		// The R04 regression: this POST must be registered.
		{"POST", "/api/v1/sdk/templates/compat/tpl-1/adopt-baseline"},
	}

	for _, rt := range routes {
		// fakeCM returns errFakeNotConfigured for unconfigured methods, which
		// makes the handler return 502 — NOT 404. So any non-404 status proves
		// the route is registered.
		w := httptestRecorder(t, r, rt.method, rt.path)
		if w.Code == http.StatusNotFound {
			t.Errorf("route not registered: %s %s (got 404)", rt.method, rt.path)
		}
	}
}

// ── R12: long operation timeout ───────────────────────────────────────────

// TestR12_LongOpTimeoutIs240s verifies that the long-operation timeout
// constant is 240 seconds, matching the original CubeAPI budget for
// synchronous snapshot create/rollback and template delete. R12 fix: the
// new CubeMaster client must not impose a shorter fixed timeout.
func TestR12_LongOpTimeoutIs240s(t *testing.T) {
	if longOpTimeout != 240*time.Second {
		t.Errorf("longOpTimeout = %v, want 240s", longOpTimeout)
	}
}

// TestR12_LongOpCtxSetsDeadline verifies that longOpCtx produces a context
// with a deadline approximately 240s in the future. This proves the long-op
// handlers (CreateSnapshot, RollbackSandbox, DeleteTemplate) get a 240s
// budget instead of the old 30s client-level timeout.
func TestR12_LongOpCtxSetsDeadline(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	// longOpCtx derives from c.Request.Context(); attach a request so
	// c.Request.Context() doesn't panic.
	c.Request = httptest.NewRequest("GET", "/", nil)
	ctx, cancel := longOpCtx(c)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("longOpCtx returned context with no deadline")
	}
	remaining := time.Until(dl)
	// Should be close to 240s (allow some slack for test execution).
	if remaining < 230*time.Second || remaining > 240*time.Second {
		t.Errorf("longOpCtx deadline remaining = %v, want ~240s", remaining)
	}
}

// TestR12_LongOpHandlerUsesLongContext verifies that CreateSnapshot (a
// long-op handler) passes a context with the 240s deadline to the CubeMaster
// client, not the default request context. The fake CM captures the context
// deadline and the test asserts it's ~240s, proving R12: long operations
// get the full 240s budget, not a 30s cap.
func TestR12_LongOpHandlerUsesLongContext(t *testing.T) {
	var capturedCtx context.Context
	cm := &fakeCM{
		createSnapshot: func(ctx context.Context, _ interface{}) (json.RawMessage, error) {
			capturedCtx = ctx
			return raw(`{"ret":{"ret_code":0},"snapshot_id":"snap-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "POST", "/api/v1/sdk/sandboxes/sb-1/snapshots", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	dl, ok := capturedCtx.Deadline()
	if !ok {
		t.Fatal("CreateSnapshot did not pass a context with a deadline to CM client")
	}
	remaining := time.Until(dl)
	// R12 fix: the deadline should be ~240s, NOT 30s.
	if remaining < 230*time.Second {
		t.Errorf("CreateSnapshot context deadline remaining = %v, want >=230s (R12: 240s budget, not 30s)", remaining)
	}
}

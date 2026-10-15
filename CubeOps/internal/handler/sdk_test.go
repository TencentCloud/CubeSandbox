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
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
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
	if items[0]["cpuCount"] != float64(2) {
		t.Errorf("cpuCount = %v, want 2", items[0]["cpuCount"])
	}
	if items[0]["cpuMilli"] != float64(2000) {
		t.Errorf("cpuMilli = %v, want 2000", items[0]["cpuMilli"])
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

func TestSDK_ListSandboxes_PrefersExactResourceUnits(t *testing.T) {
	cm := &fakeCM{
		listSandboxesWithBody: func(_ context.Context, _ interface{}) (json.RawMessage, error) {
			return raw(`{
				"ret": {"ret_code": 0},
				"data": [{
					"sandbox_id": "sb-small", "host_id": "node-a",
					"cpu_count": 0, "memory_mb": 525,
					"cpu_milli": 100, "memory_mib": 500,
					"annotations": {}, "labels": {}
				}]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var items []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal array: %v body=%s", err, w.Body.String())
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0]["cpuCount"] != float64(0) {
		t.Errorf("cpuCount = %v, want 0", items[0]["cpuCount"])
	}
	if items[0]["cpuMilli"] != float64(100) {
		t.Errorf("cpuMilli = %v, want 100", items[0]["cpuMilli"])
	}
	if items[0]["memoryMB"] != float64(500) {
		t.Errorf("memoryMB = %v, want 500", items[0]["memoryMB"])
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
					"containers": [{"container_id": "c-1", "cpu": "2000m", "mem": "2048Mi", "cpu_milli": 2000, "memory_mib": 2048, "create_at": 1700000000000000000, "type": "sandbox"}],
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
	if detail["cpuCount"] != float64(2) {
		t.Errorf("cpuCount = %v, want 2", detail["cpuCount"])
	}
	if detail["cpuMilli"] != float64(2000) {
		t.Errorf("cpuMilli = %v, want 2000", detail["cpuMilli"])
	}
	if detail["memoryMB"] != float64(2048) {
		t.Errorf("memoryMB = %v, want 2048", detail["memoryMB"])
	}
	if detail["state"] != "running" {
		t.Errorf("state = %v, want running (status 1 → running)", detail["state"])
	}
}

func TestSDK_GetSandbox_UnknownStateIsNotReportedAsRunning(t *testing.T) {
	cm := &fakeCM{
		getSandbox: func(_ context.Context, _, _ string) (json.RawMessage, error) {
			return raw(`{
				"ret": {"ret_code": 0},
				"data": [{
					"sandbox_id": "sb-unknown", "status": 3,
					"containers": [], "annotations": {}, "labels": {}
				}]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes/sb-unknown")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if detail["state"] != "unknown" {
		t.Errorf("state = %v, want unknown (CubeMaster status 3)", detail["state"])
	}
}

func TestSDK_GetSandbox_PrefersExactResourceUnits(t *testing.T) {
	cm := &fakeCM{
		getSandbox: func(_ context.Context, _, _ string) (json.RawMessage, error) {
			return raw(`{
				"ret": {"ret_code": 0},
				"data": [{
					"sandbox_id": "sb-small", "host_id": "node-a", "status": 1,
					"containers": [{"container_id": "c-1", "cpu": "500m", "mem": "2Gi", "cpu_milli": 500, "memory_mib": 2048, "type": "sandbox"}],
					"annotations": {}, "labels": {}
				}]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes/sb-small")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	// A 2Gi / 500m sandbox must report exact units, not the 0 the old lossy
	// detail parser returned for sub-core CPU and Gi-suffixed memory.
	if detail["cpuCount"] != float64(0) {
		t.Errorf("cpuCount = %v, want 0 (sub-core truncates)", detail["cpuCount"])
	}
	if detail["cpuMilli"] != float64(500) {
		t.Errorf("cpuMilli = %v, want 500", detail["cpuMilli"])
	}
	if detail["memoryMB"] != float64(2048) {
		t.Errorf("memoryMB = %v, want 2048", detail["memoryMB"])
	}
}

func TestSDK_GetSandbox_FallbackParsesRawSpec(t *testing.T) {
	// Older CubeMaster responses carry only the raw cpu/mem strings, without
	// the exact cpu_milli/memory_mib fields. The detail path must still parse
	// Gi-suffixed memory (and sub-core CPU) correctly via the fallback parsers.
	cm := &fakeCM{
		getSandbox: func(_ context.Context, _, _ string) (json.RawMessage, error) {
			return raw(`{
				"ret": {"ret_code": 0},
				"data": [{
					"sandbox_id": "sb-legacy", "host_id": "node-a", "status": 1,
					"containers": [{"container_id": "c-1", "cpu": "500m", "mem": "2Gi", "type": "sandbox"}],
					"annotations": {}, "labels": {}
				}]
			}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/sandboxes/sb-legacy")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if detail["cpuMilli"] != float64(500) {
		t.Errorf("cpuMilli = %v, want 500", detail["cpuMilli"])
	}
	if detail["memoryMB"] != float64(2048) {
		t.Errorf("memoryMB = %v, want 2048 (2Gi must not parse to 0)", detail["memoryMB"])
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

// ── DeleteSnapshot ───────────────────────────────────────────────────────

func TestSDK_DeleteSnapshot_Success(t *testing.T) {
	var capturedID string
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, snapshotID string) (json.RawMessage, error) {
			capturedID = snapshotID
			return raw(`{"ret": {"ret_code": 0}, "request_id": "r-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-99")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if capturedID != "snap-99" {
		t.Errorf("CM deleteSnapshot id = %q, want snap-99", capturedID)
	}
}

func TestSDK_ListSnapshots_PassesNextTokenHeader(t *testing.T) {
	cm := &fakeCM{
		listSnapshots: func(_ context.Context, _ map[string]string) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 0}, "data": [], "next_token": "100"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots?limit=100")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Next-Token"); got != "100" {
		t.Errorf("X-Next-Token = %q, want 100", got)
	}
}

// TestSDK_ListSnapshots_Empty verifies the SDK returns [] (not a {requestID}
// object) when CubeMaster omits `data` for an empty list.
func TestSDK_ListSnapshots_Empty(t *testing.T) {
	cm := &fakeCM{
		listSnapshots: func(_ context.Context, _ map[string]string) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 0}, "request_id": "r-1"}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots?limit=100")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

// TestSDK_ListSnapshots_NonJSONUpstream verifies a 2xx non-envelope body (e.g. a
// proxy HTML page) surfaces as 502 instead of being masked as an empty list.
func TestSDK_ListSnapshots_NonJSONUpstream(t *testing.T) {
	cm := &fakeCM{
		listSnapshots: func(_ context.Context, _ map[string]string) (json.RawMessage, error) {
			return raw(`<html>proxy error</html>`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots?limit=100")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "proxy error") {
		t.Errorf("body leaks upstream content: %s", w.Body.String())
	}
	if got := w.Header().Get("X-Next-Token"); got != "" {
		t.Errorf("X-Next-Token = %q, want empty", got)
	}
}

func TestSDK_GetSnapshot_Success(t *testing.T) {
	var capturedID string
	cm := &fakeCM{
		getSnapshot: func(_ context.Context, snapshotID string) (json.RawMessage, error) {
			capturedID = snapshotID
			// CubeMaster's real detail shape: {ret, requestID, snapshot}, not data.
			return raw(`{"ret": {"ret_code": 0}, "request_id": "r-1", "snapshot": {"snapshot_id": "snap-99", "status": "READY"}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots/snap-99")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if capturedID != "snap-99" {
		t.Errorf("CM getSnapshot id = %q, want snap-99", capturedID)
	}
	// SDK passes through the whole envelope (minus ret), keeping the snapshot
	// object nested — the frontend unwraps res.snapshot.
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	snapObj, ok := body["snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("body has no snapshot object; body=%s", w.Body.String())
	}
	if snapObj["snapshotID"] != "snap-99" {
		t.Errorf("snapshot.snapshotID = %v, want snap-99", snapObj["snapshotID"])
	}
}

func TestSDK_GetSnapshot_NotFoundInCM(t *testing.T) {
	cm := &fakeCM{
		getSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 130404, "ret_msg": "snapshot not found"}, "data": null}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots/snap-nope")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestSDK_GetSnapshot_HTTPErrorShapes verifies a not-found carried as an HTTP
// status maps to 404 like the ret_code shape, while opaque failures stay 502.
func TestSDK_GetSnapshot_HTTPErrorShapes(t *testing.T) {
	const notFoundEnvelope = `{"ret":{"ret_code":130404,"ret_msg":"snapshot not found"}}`
	cases := []struct {
		name   string
		err    error
		want   int
		wantIn string
	}{
		{"404 with not-found envelope", &cubemaster.HTTPError{Status: http.StatusNotFound, Body: notFoundEnvelope}, http.StatusNotFound, "snapshot not found"},
		{"400 with not-found envelope", &cubemaster.HTTPError{Status: http.StatusBadRequest, Body: notFoundEnvelope}, http.StatusNotFound, "snapshot not found"},
		{"bare 404 is a route problem", &cubemaster.HTTPError{Status: http.StatusNotFound, Body: "404 page not found"}, http.StatusBadGateway, ""},
		{"503 with not-found envelope", &cubemaster.HTTPError{Status: http.StatusServiceUnavailable, Body: notFoundEnvelope}, http.StatusBadGateway, ""},
		{"404 with other business code", &cubemaster.HTTPError{Status: http.StatusNotFound, Body: `{"ret":{"ret_code":130500,"ret_msg":"boom"}}`}, http.StatusBadGateway, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cm := &fakeCM{
				getSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
					return nil, tc.err
				},
			}
			r := newSDKRouter(t, cm)

			w := httptestRecorder(t, r, "GET", "/api/v1/sdk/snapshots/snap-x")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.wantIn != "" && !strings.Contains(w.Body.String(), tc.wantIn) {
				t.Errorf("body = %s, want it to contain %q", w.Body.String(), tc.wantIn)
			}
		})
	}
}

// TestSDK_SnapshotID_MalformedIsRejected verifies IDs CubeMaster never issues
// get 404 on GET and DELETE without reaching CubeMaster or AgentHub.
func TestSDK_SnapshotID_MalformedIsRejected(t *testing.T) {
	ids := []string{
		"storage", "compatible-nodes-by-factors", "..", ".", "tpl-abc",
		"agenthub-0a1b2c3d", "snap-", "snap-a%2Fb", "snap-" + strings.Repeat("a", 65),
	}
	for _, id := range ids {
		for _, method := range []string{"GET", "DELETE"} {
			t.Run(method+" "+id, func(t *testing.T) {
				calls := 0
				cm := &fakeCM{
					getSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
						calls++
						return raw(`{}`), nil
					},
					deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
						calls++
						return raw(`{}`), nil
					},
				}
				softDeletes := 0
				svc := &service.AgentHubService{Store: &stubAgentStore{
					countRefFn: func(context.Context, string) (int, error) { calls++; return 0, nil },
					softDeleteFn: func(context.Context, string) (int64, error) {
						softDeletes++
						return 0, nil
					},
				}}
				r := gin.New()
				NewSDKHandler(cm).WithAgentHubService(svc).Register(r.Group("/api/v1/sdk"))

				w := httptestRecorder(t, r, method, "/api/v1/sdk/snapshots/"+id)
				if w.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
				}
				if calls != 0 || softDeletes != 0 {
					t.Errorf("upstream reached: cm/store calls=%d softDeletes=%d, want 0", calls, softDeletes)
				}
			})
		}
	}
}

// stubAgentStore embeds service.AgentStore so only the method under test is
// overridden; any other call panics (never expected in these tests).
type stubAgentStore struct {
	service.AgentStore
	countRefFn   func(ctx context.Context, snapshotID string) (int, error)
	softDeleteFn func(ctx context.Context, snapshotID string) (int64, error)
}

func (s *stubAgentStore) CountTemplatesReferencingSnapshot(ctx context.Context, snapshotID string) (int, error) {
	return s.countRefFn(ctx, snapshotID)
}

func (s *stubAgentStore) SoftDeleteAgentSnapshotsByInfraID(ctx context.Context, snapshotID string) (int64, error) {
	if s.softDeleteFn == nil {
		return 0, nil
	}
	return s.softDeleteFn(ctx, snapshotID)
}

func (s *stubAgentStore) ClearBaseSnapshotIDBySnapshotID(context.Context, string) (int64, error) {
	return 0, nil
}

// TestSDK_DeleteSnapshot_ReverseSyncFailureWarns verifies a failed AgentHub cleanup
// keeps the success status but adds X-Cube-Warning, and a later retry clears it.
func TestSDK_DeleteSnapshot_ReverseSyncFailureWarns(t *testing.T) {
	const warnHeader = "X-Cube-Warning"
	syncFails := true
	cmDeleted := false
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			if cmDeleted {
				return nil, &cubemaster.CMError{RetCode: 130404, RetMsg: "snapshot not found"}
			}
			cmDeleted = true
			return raw(`{"ret": {"ret_code": 0}, "request_id": "r-1"}`), nil
		},
	}
	svc := &service.AgentHubService{Store: &stubAgentStore{
		countRefFn: func(_ context.Context, _ string) (int, error) { return 0, nil },
		softDeleteFn: func(_ context.Context, _ string) (int64, error) {
			if syncFails {
				return 0, errors.New("db down")
			}
			return 1, nil
		},
	}}
	r := gin.New()
	NewSDKHandler(cm).WithAgentHubService(svc).Register(r.Group("/api/v1/sdk"))

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusOK {
		t.Fatalf("first delete: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get(warnHeader) == "" {
		t.Errorf("first delete: %s header missing after a failed cleanup", warnHeader)
	}
	if strings.Contains(w.Header().Get(warnHeader), "db down") {
		t.Errorf("%s leaks the store error: %q", warnHeader, w.Header().Get(warnHeader))
	}

	w = httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("retry while cleanup still fails: status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get(warnHeader) == "" {
		t.Errorf("retry while cleanup still fails: %s header missing", warnHeader)
	}

	syncFails = false
	w = httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("reconciling retry: status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(warnHeader); got != "" {
		t.Errorf("reconciling retry: %s = %q, want empty", warnHeader, got)
	}
}

func TestSDK_DeleteSnapshot_TemplateReferenced_Returns409(t *testing.T) {
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}
	svc := &service.AgentHubService{Store: &stubAgentStore{
		countRefFn: func(_ context.Context, _ string) (int, error) { return 3, nil },
	}}
	r := gin.New()
	h := NewSDKHandler(cm).WithAgentHubService(svc)
	h.Register(r.Group("/api/v1/sdk"))

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestSDK_DeleteSnapshot_RefCountError_Returns502(t *testing.T) {
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}
	svc := &service.AgentHubService{Store: &stubAgentStore{
		countRefFn: func(_ context.Context, _ string) (int, error) { return 0, errors.New("db down") },
	}}
	r := gin.New()
	h := NewSDKHandler(cm).WithAgentHubService(svc)
	h.Register(r.Group("/api/v1/sdk"))

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

func TestSDK_DeleteSnapshot_ConflictInCM(t *testing.T) {
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			// Snapshot in CREATING/DELETING or active job → CubeMaster 130409.
			return raw(`{"ret": {"ret_code": 130409, "ret_msg": "snapshot in use"}, "data": null}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-1")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (ret_code 130409 must map to Conflict); body=%s", w.Code, w.Body.String())
	}
}

// TestSDK_DeleteSnapshot_CMNotFoundIsIdempotent verifies a CM 404 (already
// deleted out-of-band) returns 204 instead of 404.
func TestSDK_DeleteSnapshot_CMNotFoundIsIdempotent(t *testing.T) {
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			return nil, &cubemaster.CMError{RetCode: 130404, RetMsg: "snapshot not found"}
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-gone")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestSDK_DeleteSnapshot_HTTPNotFoundIsIdempotent verifies a 4xx carrying the
// 130404 envelope (HTTPError shape) is also treated as already-deleted.
func TestSDK_DeleteSnapshot_HTTPNotFoundIsIdempotent(t *testing.T) {
	cm := &fakeCM{
		deleteSnapshot: func(_ context.Context, _ string) (json.RawMessage, error) {
			return nil, &cubemaster.HTTPError{
				Status: http.StatusNotFound,
				Body:   `{"ret":{"ret_code":130404,"ret_msg":"snapshot not found"}}`,
			}
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/snapshots/snap-gone")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", w.Code, w.Body.String())
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
		{"GET", "/api/v1/sdk/snapshots/snap-1"},
		{"DELETE", "/api/v1/sdk/snapshots/snap-1"},
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

// TestR12_LongOpTimeoutIs15min verifies the long-operation timeout aligns
// with CubeMaster's snapshotOperationTimeout (15 min).
func TestR12_LongOpTimeoutIs15min(t *testing.T) {
	if longOpTimeout != 15*time.Minute {
		t.Errorf("longOpTimeout = %v, want 15m", longOpTimeout)
	}
}

// TestR12_LongOpCtxSetsDeadline verifies that longOpCtx produces a context
// with a deadline approximately 15 min in the future.
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
	// Should be close to 15 min (allow some slack for test execution).
	if remaining < 14*time.Minute || remaining > 15*time.Minute {
		t.Errorf("longOpCtx deadline remaining = %v, want ~15m", remaining)
	}
}

// TestTemplateDeleteCtx_Deadline verifies DeleteTemplate's budget is ~350s and stays
// below CubeMaster's default 360s write timeout and the snapshot handlers' budget.
func TestTemplateDeleteCtx_Deadline(t *testing.T) {
	const cmDefaultWriteTimeout = 360 * time.Second
	if templateDeleteTimeout >= cmDefaultWriteTimeout || templateDeleteTimeout >= longOpTimeout {
		t.Fatalf("templateDeleteTimeout = %v, want below %v and %v", templateDeleteTimeout, cmDefaultWriteTimeout, longOpTimeout)
	}

	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	ctx, cancel := templateDeleteCtx(c)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("templateDeleteCtx returned context with no deadline")
	}
	if remaining := time.Until(dl); remaining < 340*time.Second || remaining > templateDeleteTimeout {
		t.Errorf("templateDeleteCtx deadline remaining = %v, want ~350s", remaining)
	}
}

// TestSDK_DeleteTemplate_UsesTemplateDeleteDeadline verifies the handler passes the
// ~350s context to CubeMaster, not the 15 min snapshot one.
func TestSDK_DeleteTemplate_UsesTemplateDeleteDeadline(t *testing.T) {
	var captured context.Context
	cm := &fakeCM{
		deleteTemplate: func(ctx context.Context, _ interface{}) (json.RawMessage, error) {
			captured = ctx
			return raw(`{"ret": {"ret_code": 0}}`), nil
		},
	}
	r := newSDKRouter(t, cm)

	w := httptestRecorder(t, r, "DELETE", "/api/v1/sdk/templates/tpl-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	dl, ok := captured.Deadline()
	if !ok {
		t.Fatal("CubeMaster call got a context with no deadline")
	}
	if remaining := time.Until(dl); remaining > templateDeleteTimeout {
		t.Errorf("deadline remaining = %v, want at most %v", remaining, templateDeleteTimeout)
	}
}

// TestR12_LongOpHandlerUsesLongContext verifies CreateSnapshot passes the 15 min
// deadline context to the CubeMaster client, not the default request context.
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
	if remaining < 14*time.Minute {
		t.Errorf("CreateSnapshot context deadline remaining = %v, want >=14m (15m budget)", remaining)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/cubemaster"
)

// seedSnapshot inserts a snapshot row directly via SQL for testing.
func seedSnapshot(t *testing.T, env *testEnv, snapshotID, agentID, sandboxID, name, kind string) {
	t.Helper()
	result := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_snapshot
		   (snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		snapshotID, agentID, sandboxID, name, "ready", kind,
	)
	if result.Error != nil {
		t.Fatalf("seed snapshot: %v", result.Error)
	}
}

// ── R07: DeleteInstance treats CM "not found" as already deleted ──

// TestAgentHub_DeleteInstance_CMNotFound_ProceedsWithCleanup verifies that
// when CubeMaster returns "no such sandbox" (130404), DeleteInstance treats
// it as "already deleted" and proceeds to clean up host-side state and DB
// records. This handles orphan DB records from failed creations or expired
// sandboxes. See R07.
func TestAgentHub_DeleteInstance_CMNotFound_ProceedsWithCleanup(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-orphan", "Orphan", "sb-orphan")

	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		return nil, &cubemaster.CMError{RetCode: 130404, RetMsg: "no such sandbox"}
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-orphan", "")

	// R07 fix: not-found is treated as success → 204.
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	// R07 fix: DB record must be soft-deleted (orphan cleaned up).
	inst, err := env.store.GetInstance(t.Context(), "agent-orphan")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst != nil {
		t.Error("instance should have been soft-deleted (CM said not found); expected nil")
	}
}

// ── R07: DeleteInstance must not delete host state / DB when CubeMaster delete fails ──

// TestAgentHub_DeleteInstance_CMDeleteFails_NoDBDeletion verifies that when
// CubeMaster's DeleteSandbox returns an error, DeleteInstance does NOT proceed
// to delete the host-side state directory or soft-delete the DB record.
// Instead it returns the error to the client. See R07.
func TestAgentHub_DeleteInstance_CMDeleteFails_NoDBDeletion(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-del-fail", "DelFail", "sb-del-fail")

	// Inject a CubeMaster delete failure.
	var deleteCalled bool
	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		deleteCalled = true
		return nil, &cubemaster.CMError{RetCode: 130490, RetMsg: "sandbox is pausing; retry later"}
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-del-fail", "")

	if !deleteCalled {
		t.Fatal("expected CubeMaster DeleteSandbox to be called")
	}

	// R07 fix: should return 503 (pausing → retryable), NOT 204.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}

	// R07 fix: the DB record must still exist (not soft-deleted).
	inst, err := env.store.GetInstance(t.Context(), "agent-del-fail")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst == nil {
		t.Error("instance was soft-deleted despite CM delete failure; expected record to remain")
	}
}

// ── R07: DeleteInstance sends requestID and correct instance_type ──

// TestAgentHub_DeleteInstance_SendsRequestIDAndCubebox verifies that the
// DeleteSandbox request body includes a non-empty request_id and uses
// instance_type="cubebox" (not inst.Engine which is "openclaw"). See R07.
func TestAgentHub_DeleteInstance_SendsRequestIDAndCubebox(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-req", "ReqCheck", "sb-req")

	var capturedBody map[string]interface{}
	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &capturedBody)
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-req", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	// R07 fix: request_id must be present and non-empty.
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("request_id = %v, want non-empty string", capturedBody["requestID"])
	}
	// R07 fix: instance_type must be "cubebox", not "openclaw" (inst.Engine).
	instType, ok := capturedBody["instance_type"].(string)
	if !ok || instType != "cubebox" {
		t.Errorf("instance_type = %v, want \"cubebox\"", capturedBody["instance_type"])
	}
}

// ── R09: DeleteSnapshot returns 409 when snapshot is referenced by a template ──

// TestAgentHub_DeleteSnapshot_TemplateReferenced_Returns409 verifies that
// deleting a snapshot that is referenced by a published template returns
// 409 Conflict, and does NOT delete the DB record. See R09.
func TestAgentHub_DeleteSnapshot_TemplateReferenced_Returns409(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-snap-ref", "SnapRef", "sb-snap-ref")

	// Insert a snapshot that is referenced by a template.
	result := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_snapshot
		   (snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind,
		    rootfs_source_type, rootfs_source_id, rootfs_snapshot_id, published_template_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-referenced", "agent-snap-ref", "sb-snap-ref", "ref-snap", "ready",
		"full_snapshot", "snapshot", "snap-referenced", "snap-referenced",
		"tpl-ref-1",
	)
	if result.Error != nil {
		t.Fatalf("seed snapshot: %v", result.Error)
	}
	// Insert the referencing template.
	result = env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_template
		   (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tpl-ref-1", "Ref Template", "agent-snap-ref", "snap-referenced", "sb-snap-ref", "deepseek-v4", "1.0.0",
	)
	if result.Error != nil {
		t.Fatalf("seed template: %v", result.Error)
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-snap-ref/snapshots/snap-referenced", "")

	// R09 fix: should return 409 Conflict.
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}

	// R09 fix: snapshot must still exist in DB.
	snap, err := env.store.GetAgentSnapshot(t.Context(), "agent-snap-ref", "snap-referenced")
	if err != nil {
		t.Fatalf("GetAgentSnapshot: %v", err)
	}
	if snap == nil {
		t.Error("snapshot was deleted despite template references; expected 409 + record preserved")
	}
}

// ── R09: DeleteSnapshot calls CubeMaster for full_snapshot kind ──

// TestAgentHub_DeleteSnapshot_FullSnapshot_CallsCubeMaster verifies that
// deleting a full_snapshot kind snapshot calls CubeMaster's DeleteSnapshot
// to free physical storage, and only soft-deletes the DB record after
// physical cleanup succeeds. See R09.
func TestAgentHub_DeleteSnapshot_FullSnapshot_CallsCubeMaster(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-snap-cm", "SnapCM", "sb-snap-cm")

	seedSnapshot(t, env, "snap-full-1", "agent-snap-cm", "sb-snap-cm", "full-snap", "full_snapshot")

	var cmDeleteCalled bool
	env.fakeCM.deleteSnapshot = func(ctx context.Context, snapshotID string) (json.RawMessage, error) {
		cmDeleteCalled = true
		if snapshotID != "snap-full-1" {
			t.Errorf("CubeMaster DeleteSnapshot called with snapshotID=%q, want snap-full-1", snapshotID)
		}
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-snap-cm/snapshots/snap-full-1", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	if !cmDeleteCalled {
		t.Error("expected CubeMaster DeleteSnapshot to be called for full_snapshot kind")
	}
}

// ── R09: DeleteSnapshot does NOT soft-delete DB when physical delete fails ──

// TestAgentHub_DeleteSnapshot_CMDeleteFails_NoDBDelete verifies that if
// CubeMaster's physical snapshot delete fails, the DB record is NOT
// soft-deleted (preventing "DB invisible but storage leaked"). See R09.
func TestAgentHub_DeleteSnapshot_CMDeleteFails_NoDBDelete(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-snap-fail", "SnapFail", "sb-snap-fail")

	seedSnapshot(t, env, "snap-fail-1", "agent-snap-fail", "sb-snap-fail", "fail-snap", "full_snapshot")

	env.fakeCM.deleteSnapshot = func(ctx context.Context, snapshotID string) (json.RawMessage, error) {
		return nil, &cubemaster.CMError{RetCode: 130500, RetMsg: "internal error"}
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-snap-fail/snapshots/snap-fail-1", "")

	// Should return 502 (CMError default mapping for unknown ret_code).
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}

	// R09 fix: DB record must still exist.
	snap, err := env.store.GetAgentSnapshot(t.Context(), "agent-snap-fail", "snap-fail-1")
	if err != nil {
		t.Fatalf("GetAgentSnapshot: %v", err)
	}
	if snap == nil {
		t.Error("snapshot DB record was deleted despite CM physical delete failure; expected record to remain")
	}
}

// ── R08: rollback sends request_id ──

// TestAgentHub_Rollback_SendsRequestID verifies that the full_snapshot
// rollback path includes a non-empty request_id in the CubeMaster request.
// See R08.
func TestAgentHub_Rollback_SendsRequestID(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-rb", "Rollback", "sb-rb")
	seedSnapshot(t, env, "snap-rb-1", "agent-rb", "sb-rb", "rb-snap", "full_snapshot")

	var capturedBody map[string]interface{}
	env.fakeCM.rollbackSandbox = func(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &capturedBody)
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-rb/rollback",
		`{"snapshotID":"snap-rb-1","kind":"full_snapshot"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// R08 fix: request_id must be present and non-empty.
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("request_id = %v, want non-empty string; full body = %v", capturedBody["requestID"], capturedBody)
	}
	// R08 fix: sandbox_id must be present.
	sbID, ok := capturedBody["sandbox_id"].(string)
	if !ok || sbID != "sb-rb" {
		t.Errorf("sandbox_id = %v, want sb-rb", capturedBody["sandbox_id"])
	}
}

// ── R08 + R11: rollback returns CM error (not 502) when CubeMaster fails ──

// TestAgentHub_Rollback_CMError_MappedCorrectly verifies that a CubeMaster
// business error during rollback is mapped to the correct HTTP status (404
// for not-found), not collapsed to 502. See R08 + R11.
func TestAgentHub_Rollback_CMError_MappedCorrectly(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-rb-err", "RollbackErr", "sb-rb-err")
	seedSnapshot(t, env, "snap-rb-err", "agent-rb-err", "sb-rb-err", "err-snap", "full_snapshot")

	env.fakeCM.rollbackSandbox = func(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
		return nil, &cubemaster.CMError{RetCode: 130404, RetMsg: "sandbox not found"}
	}

	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-rb-err/rollback",
		`{"snapshotID":"snap-rb-err","kind":"full_snapshot"}`)

	// R11 fix: should return 404, not 502.
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ── R10: Agent creation failure compensates by deleting sandbox ──

// TestAgentHub_R10_CompensateDeleteOnApplyFailure verifies that when
// applyOpenclawRuntime fails (because there's no real envd in the test env),
// compensateDeleteSandbox is called to delete the already-created sandbox.
// See R10.
//
// The test seeds a market template + LLM API key so the creation path reaches
// the applyOpenclawRuntime call. CreateSandbox (fake CM) succeeds, returning
// a sandbox ID. applyOpenclawRuntime then fails (connection refused to envd).
// The handler must call DeleteSandbox as compensation before returning the
// error.
func TestAgentHub_R10_CompensateDeleteOnApplyFailure(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	// Seed LLM API key so resolveLLMConfig passes.
	if err := env.store.SetSetting(t.Context(), "llm_api_key", "test-key"); err != nil {
		t.Fatalf("seed llm_api_key: %v", err)
	}

	// Seed a market template so template resolution passes.
	result := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_template
		   (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tpl-market", "Market", "market", "", "", "deepseek-v4", "1.0.0",
	)
	if result.Error != nil {
		t.Fatalf("seed market template: %v", result.Error)
	}

	// Track CreateSandbox and DeleteSandbox calls.
	var createdSandboxID string
	var deleteCalled bool
	env.fakeCM.createSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		// Extract sandbox_id from the request to use in the response.
		b, _ := json.Marshal(body)
		var reqMap map[string]interface{}
		_ = json.Unmarshal(b, &reqMap)
		if tid, ok := reqMap["template_id"].(string); ok {
			createdSandboxID = "sb-compensate-" + tid
		}
		if createdSandboxID == "" {
			createdSandboxID = "sb-compensate"
		}
		// CubeMaster returns sandbox_id at the top level (not nested in data).
		return raw(`{"ret":{"ret_code":0},"sandbox_id":"` + createdSandboxID + `","template_id":"tpl-market"}`), nil
	}
	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		deleteCalled = true
		// Verify the compensation deletes the correct sandbox and sends
		// required fields (request_id, instance_type) so CubeMaster
		// actually processes the delete instead of rejecting it.
		b, _ := json.Marshal(body)
		var reqMap map[string]interface{}
		_ = json.Unmarshal(b, &reqMap)
		sid, _ := reqMap["sandbox_id"].(string)
		if sid != createdSandboxID {
			t.Errorf("compensation DeleteSandbox called with sandbox_id=%q, want %q", sid, createdSandboxID)
		}
		// R07/R10: request_id must be present and non-empty.
		reqID, _ := reqMap["requestID"].(string)
		if reqID == "" {
			t.Error("compensation DeleteSandbox: request_id is empty, want non-empty")
		}
		if reqID != "" && !strings.HasPrefix(reqID, "cubeops-compensate-") {
			t.Errorf("compensation DeleteSandbox: request_id = %q, want prefix \"cubeops-compensate-\"", reqID)
		}
		// R07/R10: instance_type must be "cubebox".
		instType, _ := reqMap["instance_type"].(string)
		if instType != "cubebox" {
			t.Errorf("compensation DeleteSandbox: instance_type = %q, want \"cubebox\"", instType)
		}
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	// POST create instance — applyOpenclawRuntime will fail because there's
	// no real envd listening in the test environment.
	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances",
		`{"name":"CompensateTest","engine":"openclaw","templateId":"tpl-market"}`)

	// The handler should return an error (apply failed).
	if w.Code == http.StatusCreated {
		t.Error("expected creation to fail (apply should fail), got 201")
	}

	// R10 fix: compensateDeleteSandbox must have been called.
	if !deleteCalled {
		t.Error("expected DeleteSandbox to be called as compensation after apply failure, but it was not")
	}

	// R10 fix: no DB record should exist (instance was not persisted).
	inst, _ := env.store.GetInstance(t.Context(), "")
	_ = inst // Instance won't exist because we don't know the agent ID.
	// Verify via DB that no instance with this sandbox exists.
	var count int
	row := env.store.DB().WithContext(t.Context()).Raw(
		`SELECT COUNT(*) FROM t_agenthub_instance WHERE sandbox_id = ? AND deleted_at IS NULL`,
		createdSandboxID,
	).Row()
	_ = row.Scan(&count)
	if count > 0 {
		t.Errorf("expected no DB instance record for sandbox %s, found %d", createdSandboxID, count)
	}
}

// ── R08: recover path sends request_id ──

// seedHealthySnapshot inserts a snapshot row marked as healthy so
// LatestHealthySnapshot can find it. The default seedSnapshot leaves
// is_healthy=0, which the recover path's lookup skips.
func seedHealthySnapshot(t *testing.T, env *testEnv, snapshotID, agentID, sandboxID, name, kind string) {
	t.Helper()
	result := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_snapshot
		   (snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, is_healthy)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		snapshotID, agentID, sandboxID, name, "ready", kind,
	)
	if result.Error != nil {
		t.Fatalf("seed healthy snapshot: %v", result.Error)
	}
}

// TestAgentHub_Recover_SendsRequestID verifies that the recover path's
// full_snapshot rollback includes a non-empty request_id in the CubeMaster
// request. The recover flow first tries to restart OpenClaw via envd (which
// fails in the test env — connection refused), then falls back to rolling back
// to the latest healthy snapshot. See R08.
func TestAgentHub_Recover_SendsRequestID(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-rec", "Recover", "sb-rec")
	seedHealthySnapshot(t, env, "snap-rec-1", "agent-rec", "sb-rec", "rec-snap", "full_snapshot")

	var capturedBody map[string]interface{}
	env.fakeCM.rollbackSandbox = func(ctx context.Context, sandboxID string, body interface{}) (json.RawMessage, error) {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &capturedBody)
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	// The recover endpoint will fail at step 4 (post-rollback envd restart)
	// because there's no real envd, but that's fine — we only need to verify
	// the rollback request body was correct.
	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-rec/recover", "")

	// R08 fix: request_id must be present and non-empty.
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("request_id = %v, want non-empty string; full body = %v", capturedBody["requestID"], capturedBody)
	}
	// R08 fix: the request_id must have the "cubeops-recover-" prefix.
	if reqID != "" && !strings.HasPrefix(reqID, "cubeops-recover-") {
		t.Errorf("request_id = %q, want prefix \"cubeops-recover-\"", reqID)
	}
	// R08 fix: sandbox_id must be present.
	sbID, ok := capturedBody["sandbox_id"].(string)
	if !ok || sbID != "sb-rec" {
		t.Errorf("sandbox_id = %v, want sb-rec", capturedBody["sandbox_id"])
	}
	// snapshot_id must be the healthy snapshot we seeded.
	snapID, ok := capturedBody["snapshot_id"].(string)
	if !ok || snapID != "snap-rec-1" {
		t.Errorf("snapshot_id = %v, want snap-rec-1", capturedBody["snapshot_id"])
	}

	// The response will be 500 because post-rollback restart fails, but
	// that doesn't affect our assertion — the request_id was already sent.
	_ = w
}

// ── R08: publish-template path sends request_id ──

// TestAgentHub_PublishTemplate_SendsRequestID verifies that the publish-template
// path's full_snapshot CreateSnapshot request includes a non-empty request_id.
// See R08.
func TestAgentHub_PublishTemplate_SendsRequestID(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-pub-req", "Publisher", "sb-pub-req")

	var capturedBody map[string]interface{}
	env.fakeCM.createSnapshot = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &capturedBody)
		return raw(`{"ret":{"ret_code":0},"snapshot_id":"snap-pub-req-1"}`), nil
	}

	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-pub-req/publish-template", `{"name":"My Template"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	// R08 fix: request_id must be present and non-empty.
	reqID, ok := capturedBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("request_id = %v, want non-empty string; full body = %v", capturedBody["requestID"], capturedBody)
	}
	// R08 fix: the request_id must have the "cubeops-publish-" prefix.
	if reqID != "" && !strings.HasPrefix(reqID, "cubeops-publish-") {
		t.Errorf("request_id = %q, want prefix \"cubeops-publish-\"", reqID)
	}
	// R08 fix: sandbox_id must be present.
	sbID, ok := capturedBody["sandbox_id"].(string)
	if !ok || sbID != "sb-pub-req" {
		t.Errorf("sandbox_id = %v, want sb-pub-req", capturedBody["sandbox_id"])
	}
	// instance_type must be "cubebox".
	instType, ok := capturedBody["instance_type"].(string)
	if !ok || instType != "cubebox" {
		t.Errorf("instance_type = %v, want \"cubebox\"", capturedBody["instance_type"])
	}
}

// ── S3: CloneAgent apply failure sends correct requestID ──

// TestAgentHub_CloneAgent_ApplyFailureSendsRequestID verifies that when
// CloneAgent's applyOpenclawRuntime fails, the compensation DeleteSandbox
// request uses "requestID" (not "RequestID" or "request_id") with the
// "cubeops-clone-" prefix.
//
// The old code at agenthub.go:~1958 used "RequestID" (wrong case). See
// review S3.
func TestAgentHub_CloneAgent_ApplyFailureSendsRequestID(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	// Seed a source instance.
	seedInstance(t, env.store, "agent-clone-src", "CloneSource", "sb-clone-src")

	// Seed LLM API key so resolveLLMConfig passes.
	if err := env.store.SetSetting(t.Context(), "llm_api_key", "test-key"); err != nil {
		t.Fatalf("seed llm_api_key: %v", err)
	}

	// Seed a template so template resolution works.
	result := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_template
		   (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tpl-clone", "CloneTpl", "agent-clone-src", "snap-clone-1", "sb-clone-src", "deepseek-v4", "1.0.0",
	)
	if result.Error != nil {
		t.Fatalf("seed template: %v", result.Error)
	}

	// Track CreateSandbox and DeleteSandbox calls.
	var createdSandboxID string
	var deleteBody map[string]interface{}
	var deleteCalled bool
	env.fakeCM.createSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		createdSandboxID = "sb-clone-created"
		return raw(`{"ret":{"ret_code":0},"sandbox_id":"` + createdSandboxID + `"}`), nil
	}
	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		deleteCalled = true
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &deleteBody)
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	// POST clone — applyOpenclawRuntime will fail because there's no real
	// envd listening in the test environment, triggering the compensation
	// path at agenthub.go:~1984.
	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-clone-src/clone",
		`{"name":"CloneTarget"}`)

	// The handler should return an error (apply failed → 502).
	if w.Code < 400 {
		t.Errorf("expected error status (apply should fail), got %d; body=%s", w.Code, w.Body.String())
	}

	// S3 fix: compensation DeleteSandbox must have been called.
	if !deleteCalled {
		t.Fatal("expected DeleteSandbox to be called as compensation after clone apply failure, but it was not (S3)")
	}

	// S3 fix: must use "requestID" (not "request_id" or "RequestID").
	reqID, ok := deleteBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("requestID = %v, want non-empty string; body = %v", deleteBody["requestID"], deleteBody)
	}
	if _, exists := deleteBody["request_id"]; exists {
		t.Error(`body contains "request_id" — Clone compensation must use "requestID" (S3)`)
	}
	if _, exists := deleteBody["RequestID"]; exists {
		t.Error(`body contains "RequestID" — Clone compensation must use "requestID" (S3)`)
	}

	// Prefix must be "cubeops-clone-".
	if reqID != "" && !strings.HasPrefix(reqID, "cubeops-clone-") {
		t.Errorf("requestID = %q, want prefix \"cubeops-clone-\" (S3)", reqID)
	}

	// Sandbox ID must match the created sandbox.
	sbID, _ := deleteBody["sandbox_id"].(string)
	if sbID != createdSandboxID {
		t.Errorf("sandbox_id = %q, want %q (S3)", sbID, createdSandboxID)
	}

	// Instance type must be "cubebox".
	instType, _ := deleteBody["instance_type"].(string)
	if instType != "cubebox" {
		t.Errorf("instance_type = %q, want \"cubebox\" (S3)", instType)
	}

	// No DB record should exist for the clone (apply failed before upsert).
	var count int
	row := env.store.DB().WithContext(t.Context()).Raw(
		`SELECT COUNT(*) FROM t_agenthub_instance WHERE sandbox_id = ? AND deleted_at IS NULL`,
		createdSandboxID,
	).Row()
	_ = row.Scan(&count)
	if count > 0 {
		t.Errorf("expected no DB instance record for sandbox %s, found %d (S3)", createdSandboxID, count)
	}
}

// ── S3: CloneAgent UpsertInstance failure compensates ──

// TestAgentHub_CloneAgent_UpsertFailureCompensates verifies that when
// CloneAgent successfully creates the sandbox but the apply step fails,
// the compensation DeleteSandbox is called with the correct sandbox_id.
//
// The UpsertInstance-failure path (after successful apply) is harder to
// trigger in Docker tests (requires forcing a DB error after a successful
// envd apply). That path is covered by the contract test in
// s3_clone_contract_test.go. This test covers the apply-failure path
// (more common in production) and verifies the compensation deletes the
// correct sandbox and leaves no orphan DB record. See review S3.
func TestAgentHub_CloneAgent_UpsertFailureCompensates(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	// Seed a source instance.
	seedInstance(t, env.store, "agent-clone-upsert", "CloneUpsertSrc", "sb-clone-upsert")

	// Set persistence_mode to full_snapshot on the source instance
	// (avoid shared_files host directory creation in the test).
	result := env.store.DB().WithContext(t.Context()).Exec(
		`UPDATE t_agenthub_instance SET persistence_mode = 'full_snapshot' WHERE agent_id = 'agent-clone-upsert'`,
	)
	if result.Error != nil {
		t.Fatalf("set persistence_mode: %v", result.Error)
	}

	// Seed LLM API key.
	if err := env.store.SetSetting(t.Context(), "llm_api_key", "test-key"); err != nil {
		t.Fatalf("seed llm_api_key: %v", err)
	}

	// Seed a template.
	result = env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_template
		   (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tpl-upsert", "UpsertTpl", "agent-clone-upsert", "snap-upsert-1", "sb-clone-upsert", "deepseek-v4", "1.0.0",
	)
	if result.Error != nil {
		t.Fatalf("seed template: %v", result.Error)
	}

	var createdSandboxID string
	var deleteBody map[string]interface{}
	var deleteCalled bool
	env.fakeCM.createSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		createdSandboxID = "sb-clone-upsert-created"
		return raw(`{"ret":{"ret_code":0},"sandbox_id":"` + createdSandboxID + `"}`), nil
	}
	env.fakeCM.deleteSandbox = func(ctx context.Context, body interface{}) (json.RawMessage, error) {
		deleteCalled = true
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &deleteBody)
		return raw(`{"ret": {"ret_code": 0}}`), nil
	}

	// POST clone — apply will fail (no envd), triggering compensation.
	w := doRequest(t, env, "POST", "/api/v1/agenthub/instances/agent-clone-upsert/clone",
		`{"name":"CloneUpsertTarget"}`)

	if w.Code < 400 {
		t.Errorf("expected error status, got %d; body=%s", w.Code, w.Body.String())
	}

	if !deleteCalled {
		t.Fatal("expected DeleteSandbox to be called as compensation after clone failure (S3)")
	}

	reqID, ok := deleteBody["requestID"].(string)
	if !ok || reqID == "" {
		t.Errorf("requestID = %v, want non-empty; body = %v", deleteBody["requestID"], deleteBody)
	}
	if reqID != "" && !strings.HasPrefix(reqID, "cubeops-clone-") {
		t.Errorf("requestID = %q, want prefix \"cubeops-clone-\" (S3)", reqID)
	}

	// No DB record for the clone sandbox.
	var count int
	row := env.store.DB().WithContext(t.Context()).Raw(
		`SELECT COUNT(*) FROM t_agenthub_instance WHERE sandbox_id = ? AND deleted_at IS NULL`,
		createdSandboxID,
	).Row()
	_ = row.Scan(&count)
	if count > 0 {
		t.Errorf("expected no DB instance record for sandbox %s, found %d (S3)", createdSandboxID, count)
	}
}

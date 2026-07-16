// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// seedInstance inserts a test agent instance directly via the store.
func seedInstance(t *testing.T, s *store.Store, id, name, sandboxID string) {
	t.Helper()
	inst := &store.AgentInstance{
		ID:         id,
		Name:       name,
		Status:     "running",
		Engine:     "openclaw",
		Env:        "linux",
		Model:      "deepseek-v4",
		Version:    "1.0.0",
		SandboxID:  sandboxID,
		TemplateID: "tpl-test",
		Domain:     "cube.app",
	}
	if err := s.UpsertInstance(t.Context(), inst); err != nil {
		t.Fatalf("seedInstance: %v", err)
	}
}

// ── ListInstances ─────────────────────────────────────────────────────────

func TestAgentHub_ListInstances_Empty(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	w := doRequest(t, env, "GET", "/api/v1/agenthub/instances", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var arr []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	// store.New seeds a default admin account but no agent instances.
	if len(arr) != 0 {
		t.Errorf("len(arr) = %d, want 0 (no instances seeded)", len(arr))
	}
}

func TestAgentHub_ListInstances_WithSeededData(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-1", "Alice", "sb-1")
	seedInstance(t, env.store, "agent-2", "Bob", "sb-2")

	w := doRequest(t, env, "GET", "/api/v1/agenthub/instances", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("len(arr) = %d, want 2", len(arr))
	}
}

// ── DeleteInstance ────────────────────────────────────────────────────────

func TestAgentHub_DeleteInstance_Success(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-del", "ToDelete", "sb-del")

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/agent-del", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// Verify it's gone.
	got, _ := env.store.GetInstance(t.Context(), "agent-del")
	if got != nil {
		t.Error("instance still exists after delete")
	}
}

func TestAgentHub_DeleteInstance_NotFound(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/instances/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ── ListOperations ────────────────────────────────────────────────────────

func TestAgentHub_ListOperations_Empty(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-ops", "Ops", "sb-ops")

	w := doRequest(t, env, "GET", "/api/v1/agenthub/instances/agent-ops/operations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var arr []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 0 {
		t.Errorf("len(arr) = %d, want 0", len(arr))
	}
}

// ── UpdateModel ───────────────────────────────────────────────────────────

func TestAgentHub_UpdateModel_Success(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-model", "Model", "sb-model")

	w := doRequest(t, env, "PUT", "/api/v1/agenthub/instances/agent-model/model",
		`{"model":"gpt-4"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// Verify in DB.
	inst, _ := env.store.GetInstance(t.Context(), "agent-model")
	if inst.Model != "gpt-4" {
		t.Errorf("Model = %q, want gpt-4", inst.Model)
	}
}

func TestAgentHub_UpdateModel_InvalidBody(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-model2", "Model2", "sb-model2")

	w := doRequest(t, env, "PUT", "/api/v1/agenthub/instances/agent-model2/model",
		`not json`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ── GetSettings / UpdateSettings ──────────────────────────────────────────

func TestAgentHub_GetSettings_Defaults(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	w := doRequest(t, env, "GET", "/api/v1/agenthub/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Defaults should be deepseek.
	if settings["llmProvider"] != "deepseek" {
		t.Errorf("llmProvider = %v, want deepseek", settings["llmProvider"])
	}
	if settings["persistenceEnabled"] != true {
		t.Errorf("persistenceEnabled = %v, want true", settings["persistenceEnabled"])
	}
}

func TestAgentHub_UpdateSettings_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	// Update a setting.
	w := doRequest(t, env, "PUT", "/api/v1/agenthub/settings",
		`{"llmModel":"gpt-4o","llmProvider":"openai"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if settings["llmModel"] != "gpt-4o" {
		t.Errorf("llmModel = %v, want gpt-4o", settings["llmModel"])
	}
	if settings["llmProvider"] != "openai" {
		t.Errorf("llmProvider = %v, want openai", settings["llmProvider"])
	}
}

// ── GetWecomConfig / UpdateWecomConfig ────────────────────────────────────

func TestAgentHub_WecomConfig_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()
	seedInstance(t, env.store, "agent-wecom", "Wecom", "sb-wecom")

	// Initially no wecom config — should return empty strings.
	w := doRequest(t, env, "GET", "/api/v1/agenthub/instances/agent-wecom/wecom", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Update wecom config.
	w = doRequest(t, env, "PUT", "/api/v1/agenthub/instances/agent-wecom/wecom",
		`{"botId":"bot-123","botSecret":"secret-456"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	// Read it back — botId should match, botSecret is encrypted in DB but
	// returned as plaintext by the handler.
	w = doRequest(t, env, "GET", "/api/v1/agenthub/instances/agent-wecom/wecom", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET after PUT status = %d, want 200", w.Code)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg["botId"] != "bot-123" {
		t.Errorf("botId = %v, want bot-123", cfg["botId"])
	}
}

// ── ListTemplates ─────────────────────────────────────────────────────────

func TestAgentHub_ListTemplates_Empty(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	w := doRequest(t, env, "GET", "/api/v1/agenthub/templates", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var arr []interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 0 {
		t.Errorf("len(arr) = %d, want 0", len(arr))
	}
}

func TestAgentHub_DeleteTemplate_Success(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	// Seed a template directly.
	if err := env.store.DB().WithContext(t.Context()).Exec(
		`INSERT INTO t_agenthub_template (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, 'market', '', '', ?, ?)`,
		"tpl-del", "ToDelete", "deepseek-v4", "1.0",
	).Error; err != nil {
		t.Fatalf("seed template: %v", err)
	}

	w := doRequest(t, env, "DELETE", "/api/v1/agenthub/templates/tpl-del", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	// Verify deleted.
	tmpl, _ := env.store.GetAgentTemplate(t.Context(), "tpl-del")
	if tmpl != nil {
		t.Error("template still exists after delete")
	}
}

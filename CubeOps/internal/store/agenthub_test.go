// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// TestStore_InstanceCRUD exercises the full instance lifecycle against a real
// MySQL database: Upsert → Get → List → UpdateStatus → SoftDelete.
func TestStore_InstanceCRUD(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	// 1. Upsert a new instance.
	inst := &store.AgentInstance{
		ID:         "agent-test-1",
		Name:       "Test Agent",
		Status:     "running",
		Engine:     "openclaw",
		Env:        "linux",
		Model:      "deepseek-v4",
		Version:    "1.0.0",
		SandboxID:  "sb-test-1",
		TemplateID: "tpl-test-1",
		Domain:     "cube.app",
	}
	if err := s.UpsertInstance(ctx, inst); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}

	// 2. Get it back.
	got, err := s.GetInstance(ctx, "agent-test-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got == nil {
		t.Fatal("GetInstance returned nil")
	}
	if got.Name != "Test Agent" {
		t.Errorf("Name = %q, want Test Agent", got.Name)
	}
	if got.Status != "running" {
		t.Errorf("Status = %q, want running", got.Status)
	}
	if got.SandboxID != "sb-test-1" {
		t.Errorf("SandboxID = %q, want sb-test-1", got.SandboxID)
	}

	// 3. List should include it.
	list, err := s.ListInstances(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	found := false
	for _, i := range list {
		if i.ID == "agent-test-1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("agent-test-1 not found in ListInstances")
	}

	// 3a. ListInstances pagination: limit=0 should fall back to
	// DefaultListLimit, and a tight limit should cap the result.
	page, err := s.ListInstances(ctx, 1, 0)
	if err != nil {
		t.Fatalf("ListInstances(1, 0): %v", err)
	}
	if len(page) != 1 {
		t.Errorf("ListInstances(limit=1) returned %d rows, want 1", len(page))
	}
	page, err = s.ListInstances(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListInstances(0, 0): %v", err)
	}
	if len(page) > store.DefaultListLimit {
		t.Errorf("ListInstances(limit=0) returned %d rows, want <= DefaultListLimit (%d)", len(page), store.DefaultListLimit)
	}
	// Negative offset must be clamped to 0, not error.
	page, err = s.ListInstances(ctx, 0, -5)
	if err != nil {
		t.Fatalf("ListInstances(0, -5): %v", err)
	}
	if len(page) == 0 {
		t.Error("ListInstances with negative offset should still return rows, got 0")
	}

	// 4. UpdateStatus.
	if err := s.UpdateInstanceStatus(ctx, "agent-test-1", "stopped"); err != nil {
		t.Fatalf("UpdateInstanceStatus: %v", err)
	}
	got, _ = s.GetInstance(ctx, "agent-test-1")
	if got.Status != "stopped" {
		t.Errorf("Status after update = %q, want stopped", got.Status)
	}

	// 5. SoftDelete.
	if err := s.SoftDeleteInstance(ctx, "agent-test-1"); err != nil {
		t.Fatalf("SoftDeleteInstance: %v", err)
	}
	got, _ = s.GetInstance(ctx, "agent-test-1")
	if got != nil {
		t.Error("GetInstance after soft delete should return nil")
	}
}

// TestStore_SettingsCRUD exercises the settings table round-trip.
func TestStore_SettingsCRUD(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	// Set + Get.
	if err := s.SetSetting(ctx, "test_key", "test_value"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	val, err := s.GetSetting(ctx, "test_key")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if val != "test_value" {
		t.Errorf("GetSetting = %q, want test_value", val)
	}

	// Missing key returns empty.
	val, _ = s.GetSetting(ctx, "nonexistent_key")
	if val != "" {
		t.Errorf("GetSetting nonexistent = %q, want empty", val)
	}

	// Overwrite.
	if err := s.SetSetting(ctx, "test_key", "updated_value"); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	val, _ = s.GetSetting(ctx, "test_key")
	if val != "updated_value" {
		t.Errorf("GetSetting after overwrite = %q, want updated_value", val)
	}
}

// TestStore_UserPassword verifies the user password round-trip, including
// bcrypt hashing (the store stores hashes, not plaintext).
func TestStore_UserPassword(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	// store.New seeds a default admin/admin account, so we can read it.
	hashed, err := s.GetUserPassword(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserPassword(admin): %v", err)
	}
	if hashed == "" {
		t.Fatal("default admin password hash is empty — seedDefaultAdmin may have failed")
	}

	// Set a new password and verify it's different (hashed).
	newHash, err := crypto.HashPassword("newpass")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := s.SetUserPassword(ctx, "admin", newHash); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	stored, _ := s.GetUserPassword(ctx, "admin")
	if stored != newHash {
		t.Error("SetUserPassword did not persist the new hash")
	}
	if stored == hashed {
		t.Error("new hash is the same as old — password was not actually changed")
	}
}

// TestStore_AgentTemplate exercises the agenthub template table.
func TestStore_AgentTemplate(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	// Insert a template directly via DB (the handler does this with raw SQL).
	if err := s.DB().WithContext(ctx).Exec(
		`INSERT INTO t_agenthub_template (template_id, name, source_agent_id, source_snapshot_id, source_sandbox_id, model, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"tpl-test-2", "Test Template", "market", "", "", "deepseek-v4", "1.0",
	).Error; err != nil {
		t.Fatalf("insert template: %v", err)
	}

	// GetAgentTemplate.
	tmpl, err := s.GetAgentTemplate(ctx, "tpl-test-2")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if tmpl == nil {
		t.Fatal("GetAgentTemplate returned nil")
	}
	if tmpl.Name != "Test Template" {
		t.Errorf("Name = %q, want Test Template", tmpl.Name)
	}
	if tmpl.SourceAgentID != "market" {
		t.Errorf("SourceAgentID = %q, want market", tmpl.SourceAgentID)
	}

	// ListAgentTemplates.
	list, err := s.ListAgentTemplates(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListAgentTemplates: %v", err)
	}
	found := false
	for _, t := range list {
		if t.TemplateID == "tpl-test-2" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tpl-test-2 not found in ListAgentTemplates")
	}

	// ListAgentTemplates pagination: same bounds as ListInstances.
	page, err := s.ListAgentTemplates(ctx, 1, 0)
	if err != nil {
		t.Fatalf("ListAgentTemplates(1, 0): %v", err)
	}
	if len(page) != 1 {
		t.Errorf("ListAgentTemplates(limit=1) returned %d rows, want 1", len(page))
	}
	page, err = s.ListAgentTemplates(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListAgentTemplates(0, 0): %v", err)
	}
	if len(page) > store.DefaultListLimit {
		t.Errorf("ListAgentTemplates(limit=0) returned %d rows, want <= DefaultListLimit (%d)", len(page), store.DefaultListLimit)
	}

	// DeleteAgentTemplate.
	if err := s.DeleteAgentTemplate(ctx, "tpl-test-2"); err != nil {
		t.Fatalf("DeleteAgentTemplate: %v", err)
	}
	tmpl, _ = s.GetAgentTemplate(ctx, "tpl-test-2")
	if tmpl != nil {
		t.Error("GetAgentTemplate after delete should return nil")
	}
}

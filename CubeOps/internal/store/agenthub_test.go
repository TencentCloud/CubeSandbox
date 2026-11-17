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

// TestStore_GetRecommendedAgentTemplate covers the query AgentHub uses to pick
// a default template: nothing recommended by default (registration never sets
// the flag), the marked row once PATCH sets it — even when a newer unmarked
// one exists — the newest one when several are marked, and no resurrection
// after a soft delete.
func TestStore_GetRecommendedAgentTemplate(t *testing.T) {
	env := newTestStore(t)
	defer env.teardown()
	s := env.store
	ctx := context.Background()

	// Seed through UpsertTemplateSQL, the write PublishTemplate actually uses,
	// so the first assertion pins that path rather than the column default: if
	// publishing ever started marking templates recommended, it fails here. The
	// market path's INSERT lives in the handler and is not covered by this test.
	insert := func(id string) {
		t.Helper()
		if err := s.DB().WithContext(ctx).Exec(store.UpsertTemplateSQL(),
			id, id, "agent-src", "snap-src", "sb-src", "deepseek-v4", "1.0", nil,
		).Error; err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("tpl-rec-old")
	insert("tpl-rec-new")

	// Publishing does not set the flag, so nothing is recommended yet.
	got, err := s.GetRecommendedAgentTemplate(ctx)
	if err != nil {
		t.Fatalf("GetRecommendedAgentTemplate: %v", err)
	}
	if got != nil {
		t.Fatalf("got %q, want nil — publishing must not mark a template recommended", got.TemplateID)
	}

	// Mark the older row, the way PATCH /agenthub/templates/{id} does. It must
	// win over the newer, unmarked one.
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_agenthub_template SET recommended = ? WHERE template_id = ?`, true, "tpl-rec-old",
	).Error; err != nil {
		t.Fatalf("mark recommended: %v", err)
	}
	got, err = s.GetRecommendedAgentTemplate(ctx)
	if err != nil {
		t.Fatalf("GetRecommendedAgentTemplate: %v", err)
	}
	if got == nil || got.TemplateID != "tpl-rec-old" {
		t.Fatalf("got %v, want tpl-rec-old", got)
	}
	if !got.Recommended {
		t.Error("Recommended = false on the row returned by the recommended query")
	}

	// Nothing stops an operator marking several, since the toggle is per
	// template and clears nothing else. With more than one marked, the ORDER BY
	// decides — the newest wins, and with both rows written in the same second
	// it is the id tiebreaker that settles it.
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_agenthub_template SET recommended = ? WHERE template_id = ?`, true, "tpl-rec-new",
	).Error; err != nil {
		t.Fatalf("mark second recommended: %v", err)
	}
	got, err = s.GetRecommendedAgentTemplate(ctx)
	if err != nil {
		t.Fatalf("GetRecommendedAgentTemplate: %v", err)
	}
	if got == nil || got.TemplateID != "tpl-rec-new" {
		t.Fatalf("got %v, want tpl-rec-new — the newest of several marked rows must win", got)
	}
	if err := s.DB().WithContext(ctx).Exec(
		`UPDATE t_agenthub_template SET recommended = ? WHERE template_id = ?`, false, "tpl-rec-new",
	).Error; err != nil {
		t.Fatalf("unmark second recommended: %v", err)
	}

	// A soft-deleted recommendation must not come back.
	if err := s.DeleteAgentTemplate(ctx, "tpl-rec-old"); err != nil {
		t.Fatalf("DeleteAgentTemplate: %v", err)
	}
	got, err = s.GetRecommendedAgentTemplate(ctx)
	if err != nil {
		t.Fatalf("GetRecommendedAgentTemplate: %v", err)
	}
	if got != nil {
		t.Errorf("got %q after soft delete, want nil", got.TemplateID)
	}
}

// TestStore_CountTemplatesReferencingSnapshot covers direct and indirect (via
// agenthub_state rows) template references, plus soft-delete and ID-shape edges.
func TestStore_CountTemplatesReferencingSnapshot(t *testing.T) {
	withWarehouseStores(t, func(t *testing.T, s *store.Store) {
		testCountTemplatesReferencingSnapshot(t, s)
	})
}

func testCountTemplatesReferencingSnapshot(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()

	count := func(id string) int {
		t.Helper()
		n, err := s.CountTemplatesReferencingSnapshot(ctx, id)
		if err != nil {
			t.Fatalf("CountTemplatesReferencingSnapshot(%q): %v", id, err)
		}
		return n
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if err := s.DB().WithContext(ctx).Exec(query, args...).Error; err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	seedSnapshot := func(id, kind, rootfsType, rootfsID string) {
		t.Helper()
		exec(`INSERT INTO t_agenthub_snapshot
		        (snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind,
		         rootfs_source_type, rootfs_source_id, rootfs_snapshot_id)
		      VALUES (?, 'agent-1', 'sb-1', ?, 'ready', ?, ?, ?, ?)`,
			id, id, kind, rootfsType, rootfsID, rootfsID)
	}
	seedTemplate := func(id, sourceSnapshotID string) {
		t.Helper()
		exec(store.UpsertTemplateSQL(),
			id, id, "agent-1", sourceSnapshotID, "sb-1", "deepseek-v4", "1.0", nil)
	}

	const infraID = "snap-aaaaaaaaaaaaaaaaaaaaaaaa"
	if got := count(infraID); got != 0 {
		t.Fatalf("no references: got %d, want 0", got)
	}

	// Direct: template published from the infra snapshot row itself. The row's
	// rootfs_snapshot_id equals its own ID, so both branches match; count once.
	seedSnapshot(infraID, "sandbox_snapshot", "snapshot", infraID)
	seedTemplate("tpl-direct", infraID)
	if got := count(infraID); got != 1 {
		t.Fatalf("direct reference: got %d, want 1", got)
	}

	// Indirect: derived rows rooted on the infra snapshot, whatever the
	// recorded rootfs_source_type is.
	seedSnapshot("agenthub-derived-snapshot", "agenthub_state", "snapshot", infraID)
	seedSnapshot("agenthub-derived-template", "agenthub_state", "template", infraID)
	seedTemplate("tpl-via-snapshot", "agenthub-derived-snapshot")
	seedTemplate("tpl-via-template", "agenthub-derived-template")
	if got := count(infraID); got != 3 {
		t.Fatalf("direct + indirect references: got %d, want 3", got)
	}

	// A soft-deleted template no longer protects the snapshot.
	if err := s.DeleteAgentTemplate(ctx, "tpl-via-snapshot"); err != nil {
		t.Fatalf("DeleteAgentTemplate: %v", err)
	}
	if got := count(infraID); got != 2 {
		t.Fatalf("after template soft delete: got %d, want 2", got)
	}

	// A soft-deleted derived row no longer links its template to the snapshot.
	if err := s.DeleteAgentSnapshot(ctx, "agent-1", "agenthub-derived-template"); err != nil {
		t.Fatalf("DeleteAgentSnapshot: %v", err)
	}
	if got := count(infraID); got != 1 {
		t.Fatalf("after derived row soft delete: got %d, want 1", got)
	}

	// Another snapshot is unaffected.
	if got := count("snap-bbbbbbbbbbbbbbbbbbbbbbbb"); got != 0 {
		t.Errorf("unrelated snapshot: got %d, want 0", got)
	}

	// A non-"snap-" ID that merely equals some rootfs_snapshot_id (a template
	// ID) takes the direct branch only, so it never matches indirectly.
	seedSnapshot("agenthub-derived-tpl-root", "agenthub_state", "template", "tpl-root")
	seedTemplate("tpl-via-tpl-root", "agenthub-derived-tpl-root")
	if got := count("tpl-root"); got != 0 {
		t.Errorf("template-shaped ID: got %d, want 0", got)
	}
}

// TestStore_SoftDeleteAgentSnapshotsByInfraID pins which rows the reverse-sync
// soft delete touches: only live rows whose own snapshot_id matches exactly.
func TestStore_SoftDeleteAgentSnapshotsByInfraID(t *testing.T) {
	withWarehouseStores(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		seed := func(id, rootfsID string) {
			t.Helper()
			if err := s.DB().WithContext(ctx).Exec(
				`INSERT INTO t_agenthub_snapshot
				   (snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind,
				    rootfs_source_type, rootfs_source_id, rootfs_snapshot_id)
				 VALUES (?, 'agent-1', 'sb-1', ?, 'ready', 'sandbox_snapshot', 'snapshot', ?, ?)`,
				id, id, rootfsID, rootfsID,
			).Error; err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
		live := func(id string) bool {
			t.Helper()
			snap, err := s.GetAgentSnapshot(ctx, "agent-1", id)
			if err != nil {
				t.Fatalf("GetAgentSnapshot(%s): %v", id, err)
			}
			return snap != nil
		}
		softDelete := func(id string) int64 {
			t.Helper()
			n, err := s.SoftDeleteAgentSnapshotsByInfraID(ctx, id)
			if err != nil {
				t.Fatalf("SoftDeleteAgentSnapshotsByInfraID(%q): %v", id, err)
			}
			return n
		}

		const target = "snap-aaaaaaaaaaaaaaaaaaaaaaaa"
		seed(target, target)
		const longer = target + "-extra" // shares target as a prefix
		seed(longer, longer)
		seed("agenthub-derived", target) // rooted on target, own ID differs
		seed("snap-gone", "snap-gone")
		// A fixed past timestamp keeps the already-deleted case independent of clock ticks.
		if err := s.DB().WithContext(ctx).Exec(
			`UPDATE t_agenthub_snapshot SET deleted_at = '2020-01-01 00:00:00' WHERE snapshot_id = ?`, "snap-gone",
		).Error; err != nil {
			t.Fatalf("pre-delete snap-gone: %v", err)
		}

		if got := softDelete(target); got != 1 {
			t.Fatalf("matching row: affected = %d, want 1", got)
		}
		if live(target) {
			t.Error("target row still live after soft delete")
		}
		for _, id := range []string{longer, "agenthub-derived"} {
			if !live(id) {
				t.Errorf("%s was soft-deleted, want untouched", id)
			}
		}
		if got := softDelete(target); got != 0 {
			t.Errorf("repeat call: affected = %d, want 0", got)
		}
		if got := softDelete("snap-gone"); got != 0 {
			t.Errorf("already deleted row: affected = %d, want 0", got)
		}
		if got := softDelete("snap-unknown"); got != 0 {
			t.Errorf("unknown id: affected = %d, want 0", got)
		}
	})
}

// TestStore_ClearBaseSnapshotIDBySnapshotID pins which instances lose their base
// pointer: only live ones pointing exactly at the given snapshot.
func TestStore_ClearBaseSnapshotIDBySnapshotID(t *testing.T) {
	withWarehouseStores(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		seed := func(agentID, baseID string) {
			t.Helper()
			if err := s.UpsertInstance(ctx, &store.AgentInstance{
				ID: agentID, Name: agentID, Status: "running", Engine: "openclaw", Env: "linux",
				Model: "deepseek-v4", Version: "1.0.0", SandboxID: "sb-" + agentID,
				TemplateID: "tpl-1", Domain: "cube.app",
			}); err != nil {
				t.Fatalf("UpsertInstance(%s): %v", agentID, err)
			}
			if baseID != "" {
				if err := s.SetBaseSnapshotID(ctx, agentID, baseID); err != nil {
					t.Fatalf("SetBaseSnapshotID(%s): %v", agentID, err)
				}
			}
		}
		base := func(agentID string) string {
			t.Helper()
			var got string
			if err := s.DB().WithContext(ctx).Raw(
				`SELECT COALESCE(base_snapshot_id, '') FROM t_agenthub_instance WHERE agent_id = ?`, agentID,
			).Row().Scan(&got); err != nil {
				t.Fatalf("read base_snapshot_id(%s): %v", agentID, err)
			}
			return got
		}
		clearBase := func(id string) int64 {
			t.Helper()
			n, err := s.ClearBaseSnapshotIDBySnapshotID(ctx, id)
			if err != nil {
				t.Fatalf("ClearBaseSnapshotIDBySnapshotID(%q): %v", id, err)
			}
			return n
		}

		const target = "snap-aaaaaaaaaaaaaaaaaaaaaaaa"
		seed("a-1", target)
		seed("a-2", target)
		seed("a-other", "snap-bbbbbbbbbbbbbbbbbbbbbbbb")
		seed("a-none", "")
		seed("a-deleted", target)
		if err := s.SoftDeleteInstance(ctx, "a-deleted"); err != nil {
			t.Fatalf("SoftDeleteInstance: %v", err)
		}

		if got := clearBase(target); got != 2 {
			t.Fatalf("affected = %d, want 2 live instances", got)
		}
		for agentID, want := range map[string]string{
			"a-1": "", "a-2": "", "a-none": "",
			"a-other":   "snap-bbbbbbbbbbbbbbbbbbbbbbbb",
			"a-deleted": target, // soft-deleted rows are left as they were
		} {
			if got := base(agentID); got != want {
				t.Errorf("base_snapshot_id(%s) = %q, want %q", agentID, got, want)
			}
		}
		if got := clearBase(target); got != 0 {
			t.Errorf("repeat call: affected = %d, want 0", got)
		}
		if got := clearBase("snap-unknown"); got != 0 {
			t.Errorf("unknown id: affected = %d, want 0", got)
		}
	})
}

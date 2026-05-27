// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveredLocalTemplateFromSnapshotPath(t *testing.T) {
	baseDir := t.TempDir()
	snapshotPath := filepath.Join(baseDir, "cubebox", "tpl-test", "2C2000M")
	configPath := filepath.Join(snapshotPath, "snapshot", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir snapshot config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write snapshot config: %v", err)
	}

	template := recoveredLocalTemplateFromSnapshotPath(snapshotPath)
	if template == nil {
		t.Fatal("expected recovered local template, got nil")
	}
	if template.TemplateID != "tpl-test" {
		t.Fatalf("expected template id tpl-test, got %q", template.TemplateID)
	}
	if template.Snapshot.Snapshot.Path != snapshotPath {
		t.Fatalf("expected snapshot path %q, got %q", snapshotPath, template.Snapshot.Snapshot.Path)
	}
	if template.Snapshot.Snapshot.ID != "2C2000M" {
		t.Fatalf("expected snapshot id 2C2000M, got %q", template.Snapshot.Snapshot.ID)
	}
}

func TestRecoveredLocalTemplateFromSnapshotPathRejectsTemporaryDir(t *testing.T) {
	baseDir := t.TempDir()
	snapshotPath := filepath.Join(baseDir, "cubebox", "tpl-test", "2C2000M.tmp")
	configPath := filepath.Join(snapshotPath, "snapshot", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir snapshot config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write snapshot config: %v", err)
	}

	if template := recoveredLocalTemplateFromSnapshotPath(snapshotPath); template != nil {
		t.Fatalf("expected nil for temporary snapshot path, got %+v", template)
	}
}

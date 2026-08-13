// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
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

func TestRemoveMissingLocalTemplates(t *testing.T) {
	existingPath := t.TempDir()
	missingPath := filepath.Join(t.TempDir(), "missing")
	db := &mockMetadataDB{data: make(map[string][]byte)}
	manager, err := NewCubeRunTemplateManager(db, nil)
	if err != nil {
		t.Fatalf("create local template manager: %v", err)
	}

	templates := []*templatetypes.LocalRunTemplate{
		newLocalRunTemplateForPath("tpl-existing", existingPath),
		newLocalRunTemplateForPath("snap-missing", missingPath),
		newLocalRunTemplateForPath("tpl-missing", missingPath),
		newLocalRunTemplateForPath("tpl-no-path", ""),
	}
	for _, template := range templates {
		if err := manager.store.Update(template); err != nil {
			t.Fatalf("seed template %s: %v", template.TemplateID, err)
		}
	}

	if err := manager.removeMissingLocalTemplates(context.Background()); err != nil {
		t.Fatalf("remove missing local templates: %v", err)
	}
	got, err := manager.store.ListGeneric()
	if err != nil {
		t.Fatalf("list local templates: %v", err)
	}
	gotIDs := make(map[string]bool, len(got))
	for _, template := range got {
		gotIDs[template.TemplateID] = true
	}
	if gotIDs["snap-missing"] {
		t.Fatal("missing snapshot metadata was not removed")
	}
	if gotIDs["tpl-missing"] {
		t.Fatal("missing template metadata was not removed")
	}
	if !gotIDs["tpl-existing"] {
		t.Fatal("existing template metadata was removed")
	}
	if !gotIDs["tpl-no-path"] {
		t.Fatal("template metadata without a local path was removed")
	}
}

func TestRemoveMissingLocalTemplatesKeepsTemporarySnapshot(t *testing.T) {
	snapshotPath := filepath.Join(t.TempDir(), "snapshot")
	if err := os.MkdirAll(snapshotPath+".tmp", 0o755); err != nil {
		t.Fatalf("create temporary snapshot directory: %v", err)
	}
	db := &mockMetadataDB{data: make(map[string][]byte)}
	manager, err := NewCubeRunTemplateManager(db, nil)
	if err != nil {
		t.Fatalf("create local template manager: %v", err)
	}
	template := newLocalRunTemplateForPath("tpl-publishing", snapshotPath)
	if err := manager.store.Update(template); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	if err := manager.removeMissingLocalTemplates(context.Background()); err != nil {
		t.Fatalf("remove missing local templates: %v", err)
	}
	if _, err := manager.store.GetGeneric(template.DistributionTaskID); err != nil {
		t.Fatalf("template metadata was removed while temporary snapshot exists: %v", err)
	}
}

func newLocalRunTemplateForPath(templateID, snapshotPath string) *templatetypes.LocalRunTemplate {
	return &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID:         templateID,
			DistributionTaskID: "recovered-" + templateID,
		},
		Snapshot: templatetypes.LocalSnapshot{
			Snapshot: templatetypes.Snapshot{Path: snapshotPath},
		},
	}
}

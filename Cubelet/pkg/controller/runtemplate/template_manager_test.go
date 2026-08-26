// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
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

func TestEnsureCubeRunTemplateReturnsClone(t *testing.T) {
	manager := newReadyTemplateManager(t)
	cached := seedLocalTemplate(t, manager, "tpl-clone", "task-clone", map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{Version: "v1"}},
	})

	got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-clone")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got == cached {
		t.Fatal("EnsureCubeRunTemplate returned the cached pointer")
	}
	got.Componts[templatetypes.CubeComponentCubeShim] = templatetypes.LocalComponent{
		Component: templatetypes.MachineComponent{Version: "mutated"},
	}
	stored, err := manager.store.GetGeneric("task-clone")
	if err != nil {
		t.Fatalf("get cached: %v", err)
	}
	if stored.Componts[templatetypes.CubeComponentCubeShim].Component.Version != "v1" {
		t.Fatalf("cache Componts mutated: %q", stored.Componts[templatetypes.CubeComponentCubeShim].Component.Version)
	}
}

func TestEnsureCubeRunTemplateHydratesCloneNotCache(t *testing.T) {
	manager := newReadyTemplateManager(t)
	snapshotPath := t.TempDir()
	writeSnapshotCatalog(t, snapshotPath, map[string]string{
		templatetypes.CubeComponentCubeShim: "v-from-catalog",
	})
	cached := newLocalRunTemplateForPath("tpl-hydrate", snapshotPath)
	if err := manager.store.Update(cached); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-hydrate")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got.Componts[templatetypes.CubeComponentCubeShim].Component.Version != "v-from-catalog" {
		t.Fatalf("clone was not hydrated: %+v", got.Componts)
	}
	stored, err := manager.store.GetGeneric(cached.DistributionTaskID)
	if err != nil {
		t.Fatalf("get cached: %v", err)
	}
	if _, ok := stored.Componts[templatetypes.CubeComponentCubeShim]; ok {
		t.Fatalf("hydrate wrote through to cache: %+v", stored.Componts)
	}
}

func TestListLocalTemplatesReturnsClone(t *testing.T) {
	manager := newReadyTemplateManager(t)
	seedLocalTemplate(t, manager, "tpl-list", "task-list", map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeAgent: {Component: templatetypes.MachineComponent{Version: "a1"}},
	})

	listed, err := manager.ListLocalTemplates(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := listed["tpl-list"]
	if got == nil {
		t.Fatal("missing listed template")
	}
	stored, err := manager.store.GetGeneric("task-list")
	if err != nil {
		t.Fatalf("get cached: %v", err)
	}
	if got == stored {
		t.Fatal("ListLocalTemplates returned the cached pointer")
	}
	got.Componts[templatetypes.CubeComponentCubeAgent] = templatetypes.LocalComponent{
		Component: templatetypes.MachineComponent{Version: "mutated"},
	}
	if stored.Componts[templatetypes.CubeComponentCubeAgent].Component.Version != "a1" {
		t.Fatal("list clone mutated cache Componts")
	}
}

func TestConcurrentEnsureMutatesClonesOnly(t *testing.T) {
	manager := newReadyTemplateManager(t)
	seedLocalTemplate(t, manager, "tpl-race", "task-race", map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{Version: "v1"}},
	})

	const n = 32
	var start, done sync.WaitGroup
	start.Add(n)
	done.Add(n)
	for range n {
		go func() {
			defer done.Done()
			start.Done()
			start.Wait()
			got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-race")
			if err != nil {
				t.Errorf("ensure: %v", err)
				return
			}
			got.Componts[templatetypes.CubeComponentCubeShim] = templatetypes.LocalComponent{
				Component: templatetypes.MachineComponent{Version: "g"},
			}
			listed, err := manager.ListLocalTemplates(context.Background())
			if err != nil {
				t.Errorf("list: %v", err)
				return
			}
			for _, lt := range listed {
				_ = lt.Componts[templatetypes.CubeComponentCubeShim]
			}
		}()
	}
	done.Wait()

	stored, err := manager.store.GetGeneric("task-race")
	if err != nil {
		t.Fatalf("get cached: %v", err)
	}
	if stored.Componts[templatetypes.CubeComponentCubeShim].Component.Version != "v1" {
		t.Fatalf("cache mutated under concurrency: %q", stored.Componts[templatetypes.CubeComponentCubeShim].Component.Version)
	}
}

func newReadyTemplateManager(t *testing.T) *localCubeRunTemplateManager {
	t.Helper()
	db := &mockMetadataDB{data: make(map[string][]byte)}
	manager, err := NewCubeRunTemplateManager(db, nil)
	if err != nil {
		t.Fatalf("create local template manager: %v", err)
	}
	manager.SetInstanceType("SA5")
	return manager
}

func seedLocalTemplate(t *testing.T, manager *localCubeRunTemplateManager, templateID, taskID string, componts map[string]templatetypes.LocalComponent) *templatetypes.LocalRunTemplate {
	t.Helper()
	cached := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID:         templateID,
			DistributionTaskID: taskID,
		},
		Componts: componts,
	}
	if err := manager.store.Update(cached); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return cached
}

func writeSnapshotCatalog(t *testing.T, snapshotPath string, versions map[string]string) {
	t.Helper()
	raw, err := json.Marshal(storage.SnapshotCatalogEntry{ComponentVersions: versions})
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotPath, "catalog.json"), raw, 0o644); err != nil {
		t.Fatalf("write catalog: %v", err)
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

func TestRecoveredMetadataHome(t *testing.T) {
	baseDir := t.TempDir()
	home := filepath.Join(baseDir, "tpl-s3")
	metaDir := filepath.Join(home, "metadata")
	configPath := filepath.Join(metaDir, "snapshot", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	template := recoveredS3LocalTemplate(home, metaDir)
	if template == nil {
		t.Fatal("expected recovered template")
	}
	if template.TemplateID != "tpl-s3" {
		t.Fatalf("id=%q", template.TemplateID)
	}
	if template.Snapshot.Snapshot.Path != metaDir {
		t.Fatalf("path=%q", template.Snapshot.Snapshot.Path)
	}
}

// A pause package is a run template as far as recovery is concerned: it is
// what a cross-node Resume has locally when the original tpl-* was never
// built on this node.
func TestRecoveredMetadataHomeAcceptsPausePackage(t *testing.T) {
	home := filepath.Join(t.TempDir(), "snap-pause-1")
	metaDir := filepath.Join(home, "metadata")
	if err := os.MkdirAll(filepath.Dir(metadataHomeConfigPath(home)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(metadataHomeConfigPath(home), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	template := recoveredS3LocalTemplate(home, metaDir)
	if template == nil {
		t.Fatal("expected recovered template")
	}
	if template.TemplateID != "snap-pause-1" {
		t.Fatalf("id=%q, want snap-pause-1", template.TemplateID)
	}
}

// An unmounted or absent package must not blow up the recovery attempt; the
// caller treats it as a plain miss.
func TestMountS3PackageMetadataForRecoveryTolerAtesMissingPackage(t *testing.T) {
	mountS3PackageMetadataForRecovery(context.Background(), "")
	mountS3PackageMetadataForRecovery(context.Background(), "snap-does-not-exist")
}

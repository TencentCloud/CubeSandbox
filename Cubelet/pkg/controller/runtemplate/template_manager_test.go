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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
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

	got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-clone", "xfs")
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

	got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-hydrate", "xfs")
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
			got, err := manager.EnsureCubeRunTemplate(context.Background(), "tpl-race", "xfs")
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
	t.Cleanup(manager.waitInventoryRefresh)
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

func TestS3RecoveryDoesNotPublishActiveWriter(t *testing.T) {
	manager := newReadyTemplateManager(t)
	root := t.TempDir()
	id := "tpl-building"
	home := filepath.Join(root, id)
	configPath := metadataHomeConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, release, err := storage.BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := manager.recoverMetadataHomes(context.Background(), root, "s3", id); err != nil {
		t.Fatal(err)
	}
	rows, err := manager.store.ListGeneric()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("heartbeat published a package while its writer was active")
	}
	release()
	if err := manager.recoverMetadataHomes(context.Background(), root, "s3", id); err != nil {
		t.Fatal(err)
	}
	rows, err = manager.store.ListGeneric()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("published package missing after writer completed: %d", len(rows))
	}
	// A rebuild must also hide the previous cached inventory entry.
	_, release, err = storage.BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	listed, err := manager.ListLocalTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := listed[id]; exists {
		t.Fatal("heartbeat reported a cached entry during its rebuild")
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

func TestS3PackageIDsUnderListsReusableTemplatesOnly(t *testing.T) {
	root := t.TempDir()
	snapshots := filepath.Join(root, "snapshots")
	for _, dir := range []string{
		filepath.Join(snapshots, "tpl-b"),
		filepath.Join(snapshots, "tpl-a"),
		filepath.Join(snapshots, "tpl-tmp.tmp"),
		filepath.Join(snapshots, "deadbeefdeadbeefdeadbeef"),
		filepath.Join(snapshots, "snap-pause-1"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(snapshots, "not-a-package"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got := s3PackageIDsUnder(context.Background(), snapshots, filepath.Join(root, "missing"))
	want := []string{"tpl-a", "tpl-b"}
	if len(got) != len(want) {
		t.Fatalf("ids=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids=%v, want %v", got, want)
		}
	}
}

// Heartbeat / inventory recovery must discover finalized S3 templates via
// read-only catalog probing without mounting any metadata disks, and must
// completely skip completed pause packages so idle paused sandboxes do not
// leak permanent NVMe namespaces.
func TestS3InventoryDiscoverySkipsPausePackagesAndAvoidsMounts(t *testing.T) {
	manager := newReadyTemplateManager(t)
	root := t.TempDir()
	snapshots := filepath.Join(root, storage.SnapshotKindNormal)
	pause := filepath.Join(root, storage.SnapshotKindPause)

	tplMetaDir := filepath.Join(snapshots, "tpl-sealed", storage.SnapshotMetadataDir)
	pauseMetaDir := filepath.Join(pause, "snap-paused-1", storage.SnapshotMetadataDir)
	orphanMetaDir := filepath.Join(snapshots, "tpl-orphan", storage.SnapshotMetadataDir)
	for _, dir := range []string{tplMetaDir, pauseMetaDir, orphanMetaDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Seed a previously persisted component version to verify restart recovery
	// does not wipe BoltDB component versions when derived catalog has none.
	seedLocalTemplate(t, manager, "tpl-sealed", "recovered-tpl-sealed", map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeKernel: {Component: templatetypes.MachineComponent{Version: "k-v1"}},
	})

	oldRoot := recoverySnapshotKindRoot
	oldLookup := recoveryS3CatalogLookup
	t.Cleanup(func() {
		recoverySnapshotKindRoot = oldRoot
		recoveryS3CatalogLookup = oldLookup
	})
	recoverySnapshotKindRoot = func(backend, kind string) string {
		if backend != cow.BackendS3 {
			return ""
		}
		return filepath.Join(root, kind)
	}

	var probed []string
	recoveryS3CatalogLookup = func(_ context.Context, backend, id string) (*storage.SnapshotCatalogEntry, error) {
		probed = append(probed, id)
		if backend != cow.BackendS3 {
			t.Fatalf("unexpected backend %q", backend)
		}
		if id == "snap-paused-1" {
			t.Fatalf("inventory discovery must not probe pause package %s", id)
		}
		if id == "tpl-sealed" {
			return &storage.SnapshotCatalogEntry{
				SnapshotID:   id,
				SnapshotPath: filepath.Dir(tplMetaDir),
				MetaDir:      tplMetaDir,
				RootfsVol:    "tpl-sealed-rootfs",
				MetadataVol:  "s3-meta-tpl-sealed-snap",
				Kind:         storage.CatalogKindTemplate,
				Backend:      cow.BackendS3,
			}, nil
		}
		return nil, storage.ErrSnapshotCatalogNotFound
	}

	if err := manager.recoverS3LocalTemplates(context.Background(), ""); err != nil {
		t.Fatalf("recoverS3LocalTemplates: %v", err)
	}

	// Only snapshots/tpl-* were probed; pause-snapshots/snap-paused-1 was ignored.
	for _, id := range probed {
		if id == "snap-paused-1" {
			t.Fatalf("pause package was probed during inventory scan: %v", probed)
		}
	}
	got, err := manager.cloneAndHydrate("tpl-sealed")
	if err != nil || got == nil {
		t.Fatalf("sealed S3 template not recovered: got=%v err=%v", got, err)
	}
	if got.Snapshot.Snapshot.Path != tplMetaDir {
		t.Fatalf("path=%q, want %q", got.Snapshot.Snapshot.Path, tplMetaDir)
	}
	if got.Componts[templatetypes.CubeComponentCubeKernel].Component.Version != "k-v1" {
		t.Fatalf("expected persisted component version preserved, got %+v", got.Componts)
	}
	if orphan, err := manager.cloneAndHydrate("tpl-orphan"); err != nil || orphan != nil {
		t.Fatalf("orphan dir without cubecow objects must not be published: got=%v err=%v", orphan, err)
	}
	if paused, err := manager.cloneAndHydrate("snap-paused-1"); err != nil || paused != nil {
		t.Fatalf("pause package must not be persisted into LocalTemplates store: got=%v err=%v", paused, err)
	}
}

// On-demand EnsureCubeRunTemplate for a sealed pause package (Resume fallback
// when original tpl-* is absent) resolves without mounting or polluting the
// heartbeat LocalTemplates store.
func TestEnsureCubeRunTemplateResolvesSealedPausePackageOnDemandWithoutPersisting(t *testing.T) {
	manager := newReadyTemplateManager(t)
	root := t.TempDir()
	pauseMetaDir := filepath.Join(root, storage.SnapshotKindPause, "snap-pause-demand", storage.SnapshotMetadataDir)
	if err := os.MkdirAll(pauseMetaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldRoot := recoverySnapshotKindRoot
	oldLookup := recoveryS3CatalogLookup
	t.Cleanup(func() {
		recoverySnapshotKindRoot = oldRoot
		recoveryS3CatalogLookup = oldLookup
	})
	recoverySnapshotKindRoot = func(backend, kind string) string {
		if backend != cow.BackendS3 {
			return ""
		}
		return filepath.Join(root, kind)
	}
	recoveryS3CatalogLookup = func(_ context.Context, _ string, id string) (*storage.SnapshotCatalogEntry, error) {
		if id != "snap-pause-demand" {
			return nil, storage.ErrSnapshotCatalogNotFound
		}
		return &storage.SnapshotCatalogEntry{
			SnapshotID:   id,
			SnapshotPath: filepath.Dir(pauseMetaDir),
			MetaDir:      pauseMetaDir,
			RootfsVol:    "tpl-snap-pause-demand-rootfs",
			MetadataVol:  "s3-meta-snap-pause-demand-snap",
			Kind:         storage.CatalogKindPauseSnapshot,
			Backend:      cow.BackendS3,
		}, nil
	}

	got, err := manager.EnsureCubeRunTemplate(context.Background(), "snap-pause-demand", cow.BackendS3)
	if err != nil {
		t.Fatalf("EnsureCubeRunTemplate: %v", err)
	}
	if got == nil || got.TemplateID != "snap-pause-demand" || got.Snapshot.Snapshot.Path != pauseMetaDir {
		t.Fatalf("unexpected pause template: %+v", got)
	}
	// Pause fallback must not leak into the persisted heartbeat store.
	rows, err := manager.store.ListGeneric()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected pause package not to be persisted in LocalTemplates store, got %d", len(rows))
	}
}

// ListLocalTemplates runs on the node-liveness heartbeat path and must return
// immediately even when background storage/filesystem recovery is stalled,
// coalescing repeated heartbeat triggers into at most one follow-up pass.
func TestListLocalTemplatesDoesNotBlockOnSlowInventoryRefresh(t *testing.T) {
	manager := newReadyTemplateManager(t)
	seedLocalTemplate(t, manager, "tpl-cached", "task-cached", map[string]templatetypes.LocalComponent{
		templatetypes.CubeComponentCubeShim: {Component: templatetypes.MachineComponent{Version: "v1"}},
	})

	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	manager.refreshFn = func(ctx context.Context) {
		n := runs.Add(1)
		if n == 1 {
			close(started)
			<-release
		}
	}

	// First heartbeat returns immediately even though the background refresh
	// blocks on storage.
	start := time.Now()
	listed, err := manager.ListLocalTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListLocalTemplates: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ListLocalTemplates blocked on slow refresh: %v", elapsed)
	}
	if listed["tpl-cached"] == nil {
		t.Fatal("expected cached template to be served immediately")
	}

	<-started

	// Fire 10 more heartbeats while the first refresh is still blocked:
	// every call must return promptly and coalesce into at most 1 follow-up run.
	for i := range 10 {
		callStart := time.Now()
		if _, err := manager.ListLocalTemplates(context.Background()); err != nil {
			t.Fatalf("heartbeat %d failed: %v", i, err)
		}
		if elapsed := time.Since(callStart); elapsed > time.Second {
			t.Fatalf("heartbeat %d blocked on in-flight refresh: %v", i, elapsed)
		}
	}

	close(release)
	manager.waitInventoryRefresh()

	if got := runs.Load(); got != 2 {
		t.Fatalf("expected 1 initial + 1 coalesced follow-up refresh, got %d", got)
	}
}

// A backend lookup must stay inside that backend's registered package root.
func TestEnsureCubeRunTemplateRecoveryIsolatesBackends(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		local   bool
	}{
		{name: "xfs_local", backend: "xfs", local: true},
		{name: "xfs_missing", backend: "xfs"},
		{name: "default_local", backend: "", local: true},
		{name: "jfs_local", backend: "jfs", local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newReadyTemplateManager(t)
			root := t.TempDir()
			id := "tpl-backend-isolation-" + filepath.Base(root)
			expectedBackend := tc.backend
			if expectedBackend == "" {
				expectedBackend = "xfs"
			}
			home := filepath.Join(root, "snapshots", id)
			makeHome := func(home string) {
				t.Helper()
				path := metadataHomeConfigPath(home)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.local {
				makeHome(home)
			}
			oldRoot := recoverySnapshotKindRoot
			t.Cleanup(func() { recoverySnapshotKindRoot = oldRoot })
			recoverySnapshotKindRoot = func(backend, kind string) string {
				if backend != expectedBackend {
					t.Fatalf("%s request accessed %s storage", expectedBackend, backend)
				}
				return filepath.Join(root, kind)
			}
			got, err := manager.EnsureCubeRunTemplate(context.Background(), id, tc.backend)
			if !tc.local {
				if err == nil || got != nil {
					t.Fatalf("missing template: got=%v err=%v", got, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || got.Snapshot.Snapshot.Path != filepath.Join(home, storage.SnapshotMetadataDir) {
					t.Fatalf("wrong recovered template: %+v", got)
				}
			}
		})
	}
}

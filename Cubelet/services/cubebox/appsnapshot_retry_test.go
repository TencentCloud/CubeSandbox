// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func resetAppSnapshotRetrySeams(t *testing.T) {
	t.Helper()
	oldGet := appSnapshotGetLocalSnapshot
	oldFinalize := appSnapshotFinalizeS3Package
	oldInspect := appSnapshotInspectObjects
	oldCleanupObjects := appSnapshotCleanupObjects
	oldCleanupLocal := appSnapshotCleanupTemplateLocal
	oldDelete := appSnapshotDeleteCatalog
	t.Cleanup(func() {
		appSnapshotGetLocalSnapshot = oldGet
		appSnapshotFinalizeS3Package = oldFinalize
		appSnapshotInspectObjects = oldInspect
		appSnapshotCleanupObjects = oldCleanupObjects
		appSnapshotCleanupTemplateLocal = oldCleanupLocal
		appSnapshotDeleteCatalog = oldDelete
	})
}

func completeXFSAppSnapshotEntry(t *testing.T) *storage.SnapshotCatalogEntry {
	t.Helper()
	home := t.TempDir()
	metaDir := filepath.Join(home, storage.SnapshotMetadataDir)
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "catalog.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &storage.SnapshotCatalogEntry{
		SnapshotID:      "tpl-1",
		SnapshotPath:    home,
		MetaDir:         metaDir,
		RootfsVol:       "tpl-tpl-1-rootfs",
		RootfsKind:      storage.CowKindSnapshot,
		MemoryVol:       "tpl-tpl-1-memory",
		MemoryKind:      storage.CowKindVolume,
		BuildRootfsVol:  "tpl-tpl-1-build-rootfs",
		BuildRootfsKind: storage.CowKindVolume,
		TemplateSpecFingerprint: "fp-1",
		Kind:            storage.CatalogKindTemplate,
	}
}

func TestReconcileAppSnapshotRetryReusesCompleteCatalog(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	entry := completeXFSAppSnapshotEntry(t)
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return entry, nil
	}
	appSnapshotInspectObjects = func(_ context.Context, _ string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
		out := make([]storage.CowObjectStatus, 0, len(refs))
		for _, ref := range refs {
			out = append(out, storage.CowObjectStatus{
				Name: ref.Name, Kind: ref.Kind, Role: ref.Role, Exists: true, SizeBytes: 1,
			})
		}
		return out, nil
	}
	appSnapshotCleanupObjects = func(context.Context, string, []storage.CowObjectRef) error {
		t.Fatal("complete catalog must not be cleaned")
		return nil
	}

	got, err := reconcileAppSnapshotRetryState(context.Background(), "xfs", "tpl-1", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != entry {
		t.Fatalf("entry = %+v, want existing catalog", got)
	}
}

func TestReconcileAppSnapshotRetryRejectsDifferentRequestFingerprint(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	entry := completeXFSAppSnapshotEntry(t)
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return entry, nil
	}
	appSnapshotCleanupObjects = func(context.Context, string, []storage.CowObjectRef) error {
		t.Fatal("mismatched package must not be deleted")
		return nil
	}
	_, err := reconcileAppSnapshotRetryState(context.Background(), "xfs", "tpl-1", "fp-other")
	if !errors.Is(err, storage.ErrCowObjectAlreadyExists) {
		t.Fatalf("error = %v, want ErrCowObjectAlreadyExists", err)
	}
}

func TestReconcileAppSnapshotRetryCleansIncompleteCatalog(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	entry := completeXFSAppSnapshotEntry(t)
	if err := os.Remove(filepath.Join(entry.MetaDir, "catalog.json")); err != nil {
		t.Fatal(err)
	}
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return entry, nil
	}
	var cleaned []storage.CowObjectRef
	appSnapshotCleanupObjects = func(_ context.Context, _ string, refs []storage.CowObjectRef) error {
		cleaned = append(cleaned, refs...)
		return nil
	}
	localCleaned := false
	appSnapshotCleanupTemplateLocal = func(_ context.Context, templateID, snapshotPath string) error {
		localCleaned = templateID == "tpl-1" && snapshotPath == entry.SnapshotPath
		return nil
	}
	deleted := false
	appSnapshotDeleteCatalog = func(backend, templateID string) {
		deleted = backend == "xfs" && templateID == "tpl-1"
	}

	got, err := reconcileAppSnapshotRetryState(context.Background(), "xfs", "tpl-1", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || !localCleaned || !deleted {
		t.Fatalf("got=%+v localCleaned=%v deleted=%v", got, localCleaned, deleted)
	}
	foundBuildRootfs := false
	for _, ref := range cleaned {
		if ref.Role == "build_rootfs" && ref.Name == entry.BuildRootfsVol {
			foundBuildRootfs = true
		}
	}
	if !foundBuildRootfs {
		t.Fatalf("cleanup refs do not include build rootfs: %+v", cleaned)
	}
}

func TestReconcileAppSnapshotRetryCleansIncompleteS3CatalogBeforeFinalize(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	entry := &storage.SnapshotCatalogEntry{
		SnapshotID:   "tpl-1",
		SnapshotPath: t.TempDir(),
		RootfsVol:    "tpl-tpl-1-rootfs",
		RootfsKind:   storage.CowKindSnapshot,
		MemoryVol:    "tpl-tpl-1-memory",
		MemoryKind:   storage.CowKindVolume,
		MetadataVol:  "tpl-tpl-1-metadata",
		MetadataKind: storage.CowKindVolume,
		TemplateSpecFingerprint: "fp-1",
		Kind:         storage.CatalogKindTemplate,
	}
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return entry, nil
	}
	appSnapshotInspectObjects = func(_ context.Context, _ string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
		out := make([]storage.CowObjectStatus, 0, len(refs))
		for _, ref := range refs {
			out = append(out, storage.CowObjectStatus{
				Name: ref.Name, Kind: ref.Kind, Role: ref.Role, Exists: ref.Role != "memory", SizeBytes: 1,
			})
		}
		return out, nil
	}
	appSnapshotFinalizeS3Package = func(context.Context, string, string) error {
		t.Fatal("incomplete catalog must be cleaned before finalize")
		return nil
	}
	cleaned := false
	appSnapshotCleanupObjects = func(context.Context, string, []storage.CowObjectRef) error {
		cleaned = true
		return nil
	}
	appSnapshotCleanupTemplateLocal = func(context.Context, string, string) error { return nil }
	appSnapshotDeleteCatalog = func(string, string) {}

	got, err := reconcileAppSnapshotRetryState(context.Background(), "s3", "tpl-1", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || !cleaned {
		t.Fatalf("got=%+v cleaned=%v", got, cleaned)
	}
}

func TestReconcileAppSnapshotRetryRejectsUncataloguedPublishedObjects(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return nil, storage.ErrSnapshotCatalogNotFound
	}
	appSnapshotInspectObjects = func(_ context.Context, _ string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
		out := make([]storage.CowObjectStatus, 0, len(refs))
		for _, ref := range refs {
			exists := ref.Role == "rootfs" || ref.Role == "memory"
			out = append(out, storage.CowObjectStatus{Name: ref.Name, Kind: ref.Kind, Role: ref.Role, Exists: exists, SizeBytes: 1})
		}
		return out, nil
	}
	appSnapshotCleanupObjects = func(context.Context, string, []storage.CowObjectRef) error {
		t.Fatal("ambiguous published objects must not be deleted")
		return nil
	}

	_, err := reconcileAppSnapshotRetryState(context.Background(), "xfs", "tpl-1", "fp-1")
	if !errors.Is(err, storage.ErrCowObjectAlreadyExists) {
		t.Fatalf("error = %v, want ErrCowObjectAlreadyExists", err)
	}
}

func TestReconcileAppSnapshotRetryCleansScopedHalfProduct(t *testing.T) {
	resetAppSnapshotRetrySeams(t)
	appSnapshotGetLocalSnapshot = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return nil, storage.ErrSnapshotCatalogNotFound
	}
	appSnapshotInspectObjects = func(_ context.Context, _ string, refs []storage.CowObjectRef) ([]storage.CowObjectStatus, error) {
		out := make([]storage.CowObjectStatus, 0, len(refs))
		for _, ref := range refs {
			out = append(out, storage.CowObjectStatus{
				Name: ref.Name, Kind: ref.Kind, Role: ref.Role, Exists: ref.Role == "build_rootfs", SizeBytes: 1,
			})
		}
		return out, nil
	}
	cleaned := false
	appSnapshotCleanupObjects = func(context.Context, string, []storage.CowObjectRef) error {
		cleaned = true
		return nil
	}
	appSnapshotCleanupTemplateLocal = func(context.Context, string, string) error { return nil }
	appSnapshotDeleteCatalog = func(string, string) {}

	got, err := reconcileAppSnapshotRetryState(context.Background(), "xfs", "tpl-1", "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || !cleaned {
		t.Fatalf("got=%+v cleaned=%v", got, cleaned)
	}
}

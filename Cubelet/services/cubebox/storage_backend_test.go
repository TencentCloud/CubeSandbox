// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func TestResolveRequestStorageBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", cow.BackendXFS, false},
		{"xfs", cow.BackendXFS, false},
		{"s3", cow.BackendS3, false},
		{"S3", cow.BackendS3, false},
		{"cos", "", true},
	}
	for _, tc := range cases {
		got, err := resolveRequestStorageBackend(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("resolveRequestStorageBackend(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("resolveRequestStorageBackend(%q) unexpected err: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("resolveRequestStorageBackend(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStorageBackendFromAnnotations(t *testing.T) {
	t.Parallel()
	got, err := storageBackendFromAnnotations(nil)
	if err != nil || got != cow.BackendXFS {
		t.Fatalf("nil annotations: got=%q err=%v", got, err)
	}
	got, err = storageBackendFromAnnotations(map[string]string{})
	if err != nil || got != cow.BackendXFS {
		t.Fatalf("empty annotations: got=%q err=%v", got, err)
	}
	got, err = storageBackendFromAnnotations(map[string]string{
		constants.MasterAnnotationStorageBackend: "s3",
	})
	if err != nil || got != cow.BackendS3 {
		t.Fatalf("s3 annotation: got=%q err=%v", got, err)
	}
	_, err = storageBackendFromAnnotations(map[string]string{
		constants.MasterAnnotationStorageBackend: "cos",
	})
	if err == nil {
		t.Fatal("expected error for unsupported backend")
	}
}

func TestSnapshotDirForRequest(t *testing.T) {
	t.Parallel()
	xfsRoot := storage.SnapshotKindRoot(cow.BackendXFS, storage.SnapshotKindNormal)
	s3Root := storage.SnapshotKindRoot(cow.BackendS3, storage.SnapshotKindNormal)
	if got := snapshotDirForRequest(cow.BackendXFS, ""); got != xfsRoot {
		t.Fatalf("xfs default dir=%q want %q", got, xfsRoot)
	}
	if got := snapshotDirForRequest(cow.BackendXFS, "/custom/xfs"); got != xfsRoot {
		t.Fatalf("xfs must ignore paths outside work tree, got %q", got)
	}
	if got := snapshotDirForRequest(cow.BackendS3, ""); got != s3Root {
		t.Fatalf("s3 default dir=%q, want %q", got, s3Root)
	}
	if got := snapshotDirForRequest(cow.BackendS3, constants.DefaultSnapshotDir); got != s3Root {
		t.Fatalf("s3 must not use cubetoolbox snapshot dir, got %q", got)
	}
}

func TestSnapshotWorkLayoutIsBackendAgnostic(t *testing.T) {
	t.Parallel()
	backends := []string{cow.BackendXFS, cow.BackendJFS, cow.BackendS3}
	kinds := []string{storage.SnapshotKindNormal, storage.SnapshotKindPause}
	for _, backend := range backends {
		for _, kind := range kinds {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				t.Parallel()
				layout, err := prepareSnapshotWorkLayout(backend, kind, "snap-1", "", "2C2000M")
				if err != nil {
					t.Fatal(err)
				}
				root := storage.SnapshotKindRoot(backend, kind)
				wantHome := filepath.Join(root, "snap-1")
				if layout.Home != wantHome {
					t.Fatalf("home=%q want %q", layout.Home, wantHome)
				}
				if layout.Kind != kind {
					t.Fatalf("kind=%q want %q", layout.Kind, kind)
				}
				if layout.TmpHome != layout.Home+".tmp" {
					t.Fatalf("tmpHome=%q want %q", layout.TmpHome, layout.Home+".tmp")
				}
				wantMetaDir := filepath.Join(layout.Home, storage.SnapshotMetadataDir)
				if layout.MetaDir != wantMetaDir {
					t.Fatalf("metaDir=%q want %q", layout.MetaDir, wantMetaDir)
				}
				wantMetaWork := filepath.Join(layout.TmpHome, storage.SnapshotMetadataDir)
				if layout.MetaWork != wantMetaWork {
					t.Fatalf("metaWork=%q want %q", layout.MetaWork, wantMetaWork)
				}
				if layout.MetaWork == layout.MetaDir || strings.HasPrefix(layout.MetaWork, layout.Home+string(filepath.Separator)) {
					t.Fatalf("metaWork %q must stay outside final home %q", layout.MetaWork, layout.Home)
				}
				if strings.Contains(layout.Home, "cubebox") {
					t.Fatalf("layout home %q must not use legacy cubebox nesting", layout.Home)
				}
				wantSuffix := filepath.Join(kind, "snap-1")
				if backend != cow.BackendJFS {
					wantSuffix = filepath.Join(backend, kind, "snap-1")
				}
				if !strings.HasSuffix(layout.Home, string(filepath.Separator)+wantSuffix) {
					t.Fatalf("layout home %q missing expected suffix %q", layout.Home, wantSuffix)
				}
			})
		}
	}
}

func TestSnapshotWorkLayoutPublishMovesStagedPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	home := filepath.Join(root, "snap-pub")
	layout := snapshotWorkLayout{
		Backend:  cow.BackendS3,
		Kind:     storage.SnapshotKindNormal,
		Home:     home,
		TmpHome:  home + ".tmp",
		MetaDir:  filepath.Join(home, storage.SnapshotMetadataDir),
		MetaWork: filepath.Join(home+".tmp", storage.SnapshotMetadataDir),
	}
	if err := layout.prepareWork(context.Background()); err != nil {
		t.Fatal(err)
	}
	stagedFile := filepath.Join(layout.MetaWork, "metadata.json")
	if err := os.WriteFile(stagedFile, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.publish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.TmpHome); !os.IsNotExist(err) {
		t.Fatalf("staged dir %q should be gone after publish, stat err=%v", layout.TmpHome, err)
	}
	got, err := os.ReadFile(filepath.Join(layout.MetaDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("published file=%q", got)
	}
}

func TestSnapshotWorkLayoutPublishPropagatesRemoveAllError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// os.RemoveAll on a child beneath a regular file fails with ENOTDIR even as root.
	layout := snapshotWorkLayout{
		Backend: cow.BackendS3,
		Home:    filepath.Join(blocker, "snap-pub"),
		TmpHome: filepath.Join(root, "snap-pub.tmp"),
	}
	if err := os.MkdirAll(layout.TmpHome, 0o755); err != nil {
		t.Fatal(err)
	}
	err := layout.publish()
	if err == nil || !strings.Contains(err.Error(), "remove existing snapshot home") {
		t.Fatalf("expected remove existing snapshot home error, got %v", err)
	}
	if _, statErr := os.Stat(layout.TmpHome); statErr != nil {
		t.Fatalf("staged dir should remain intact when RemoveAll fails: %v", statErr)
	}
}

func TestFinalizePublishedPackageFatalVsDeferredCleanup(t *testing.T) {
	t.Run("fatal seal error discards package and catalog", func(t *testing.T) {
		origFinalize, origDiscard := finalizePublishedPackageFn, discardSealedPackageFn
		t.Cleanup(func() {
			finalizePublishedPackageFn, discardSealedPackageFn = origFinalize, origDiscard
		})

		id := "tpl-finalize-fatal"
		home := filepath.Join(t.TempDir(), id)
		metaDir := filepath.Join(home, storage.SnapshotMetadataDir)
		if err := os.MkdirAll(metaDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := storage.WriteSnapshotCatalogFor(cow.BackendS3, &storage.SnapshotCatalogEntry{
			SnapshotID:   id,
			SnapshotPath: home,
			MetaDir:      metaDir,
			Backend:      cow.BackendS3,
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { storage.DeleteSnapshotCatalogFor(cow.BackendS3, id) })

		finalizePublishedPackageFn = func(context.Context, string, string) (storage.PackageFinalizeResult, error) {
			return storage.PackageFinalizeResult{}, errors.New("mkfs failed")
		}
		discarded := false
		discardSealedPackageFn = func(_ context.Context, backend, snapshotID string) error {
			if backend != cow.BackendS3 || snapshotID != id {
				t.Fatalf("unexpected discard args backend=%q id=%q", backend, snapshotID)
			}
			discarded = true
			return nil
		}
		fatalCleanupCalled := false
		deferredCalled := false

		err := finalizePublishedPackage(
			context.Background(), nil, cow.BackendS3, id, home,
			func() { fatalCleanupCalled = true },
			func() { deferredCalled = true },
		)
		if err == nil || !strings.Contains(err.Error(), "mkfs failed") {
			t.Fatalf("expected seal error, got %v", err)
		}
		if !fatalCleanupCalled || !discarded {
			t.Fatalf("expected fatal cleanup and sealed package discard, got cleanup=%v discarded=%v", fatalCleanupCalled, discarded)
		}
		if deferredCalled {
			t.Fatal("onCleanupDeferred must not run on fatal seal error")
		}
		if _, statErr := os.Stat(home); !os.IsNotExist(statErr) {
			t.Fatalf("snapshot home %q should be removed on fatal seal error, statErr=%v", home, statErr)
		}
		if _, catErr := storage.GetLocalSnapshotFor(context.Background(), cow.BackendS3, id); !errors.Is(catErr, storage.ErrSnapshotCatalogNotFound) {
			t.Fatalf("catalog should be deleted on fatal seal error, got err=%v", catErr)
		}
	})

	t.Run("deferred post-seal cleanup keeps package intact", func(t *testing.T) {
		origFinalize, origDiscard := finalizePublishedPackageFn, discardSealedPackageFn
		t.Cleanup(func() {
			finalizePublishedPackageFn, discardSealedPackageFn = origFinalize, origDiscard
		})

		id := "tpl-finalize-deferred"
		home := filepath.Join(t.TempDir(), id)
		metaDir := filepath.Join(home, storage.SnapshotMetadataDir)
		if err := os.MkdirAll(metaDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := storage.WriteSnapshotCatalogFor(cow.BackendS3, &storage.SnapshotCatalogEntry{
			SnapshotID:   id,
			SnapshotPath: home,
			MetaDir:      metaDir,
			Backend:      cow.BackendS3,
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { storage.DeleteSnapshotCatalogFor(cow.BackendS3, id) })

		finalizePublishedPackageFn = func(context.Context, string, string) (storage.PackageFinalizeResult, error) {
			return storage.PackageFinalizeResult{CleanupErr: errors.New("resetDir EBUSY")}, nil
		}
		discardSealedPackageFn = func(context.Context, string, string) error {
			t.Fatal("deferred cleanup must not discard sealed package")
			return nil
		}
		deferredCalled := false

		err := finalizePublishedPackage(
			context.Background(), nil, cow.BackendS3, id, home,
			func() { t.Fatal("deferred cleanup must not run fatal cleanup") },
			func() { deferredCalled = true },
		)
		if err != nil {
			t.Fatalf("expected nil error when only post-seal cleanup failed, got %v", err)
		}
		if !deferredCalled {
			t.Fatal("expected onCleanupDeferred callback to run")
		}
		if _, statErr := os.Stat(home); statErr != nil {
			t.Fatalf("snapshot home %q must be preserved on deferred cleanup: %v", home, statErr)
		}
		if _, catErr := storage.GetLocalSnapshotFor(context.Background(), cow.BackendS3, id); catErr != nil {
			t.Fatalf("catalog must be preserved on deferred cleanup: %v", catErr)
		}
	})
}

func TestCommitStagedMetadataValidationHandlesFsOnlyAndFull(t *testing.T) {
	t.Parallel()
	metaWork := t.TempDir()
	if err := os.MkdirAll(filepath.Join(metaWork, "snapshot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaWork, "metadata.json"), []byte(`{"image":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaWork, "snapshot", "config.json"), []byte(`{"cpus":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storage.ValidateStagedPackage(cow.BackendS3, metaWork, false); err != nil {
		t.Fatalf("fs-only snapshot without state.json must pass validation: %v", err)
	}
	if err := storage.ValidateStagedPackage(cow.BackendS3, metaWork, true); err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("full snapshot without state.json must fail validation, got %v", err)
	}
}

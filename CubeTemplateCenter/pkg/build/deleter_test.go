// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

// These tests cover the DB-independent parts of artifact deletion: path
// traversal guards and the local-file removal logic. The DB claim/row-removal
// flow is exercised by integration tests against a real database, since TC's
// go.mod does not carry an embedded driver (glebarez/sqlite) today.

func TestDeleteLocalArtifactRemovesManagedDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", t.TempDir())

	artifactID := "rfs-del-001"
	artifactDir := filepath.Join(root, artifactID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ext4Path := filepath.Join(artifactDir, artifactID+".ext4")
	if err := os.WriteFile(ext4Path, []byte("fake ext4 data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}

	if err := deleteLocalArtifact(artifactID, ext4Path); err != nil {
		t.Fatalf("deleteLocalArtifact: %v", err)
	}
	if _, err := os.Stat(artifactDir); !os.IsNotExist(err) {
		t.Fatalf("expected artifact dir removed, stat err=%v", err)
	}
}

func TestDeleteLocalArtifactRefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", t.TempDir())

	// A path OUTSIDE the managed root must be refused and NOT deleted.
	outsideDir := t.TempDir()
	evilPath := filepath.Join(outsideDir, "evil.ext4")
	if err := os.WriteFile(evilPath, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write evil file: %v", err)
	}

	err := deleteLocalArtifact("rfs-outside", evilPath)
	if err == nil {
		t.Fatalf("expected path-traversal refusal, got nil")
	}
	// The outside file must still exist.
	if _, statErr := os.Stat(evilPath); statErr != nil {
		t.Fatalf("expected outside file preserved, stat err=%v", statErr)
	}
}

func TestManagedArtifactDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", t.TempDir())

	inside := filepath.Join(root, "rfs-abc", "sub")
	outside := t.TempDir()

	if !managedArtifactDir(inside) {
		t.Fatalf("expected %q recognized as managed", inside)
	}
	if managedArtifactDir(outside) {
		t.Fatalf("expected %q recognized as NOT managed", outside)
	}
	// The root itself is deliberately NOT managed: deleteLocalArtifact runs
	// RemoveAll(dir), so treating the store root as managed would wipe every
	// artifact on the node (and the callback-side path validator accepts
	// exactly such top-level paths).
	if managedArtifactDir(root) {
		t.Fatalf("expected root %q recognized as NOT managed (RemoveAll would wipe the whole store)", root)
	}
}

func TestDeleterEmptyArtifactID(t *testing.T) {
	deleter := NewArtifactDeleter(nil, nil)
	if err := deleter.Delete(t.Context(), ""); err == nil {
		t.Fatalf("expected error for empty artifact_id")
	}
	if err := deleter.Delete(t.Context(), "   "); err == nil {
		t.Fatalf("expected error for whitespace-only artifact_id")
	}
}

func TestDeleteLocalArtifactFindsUnreportedFallbackResidue(t *testing.T) {
	workRoot, storeRoot, fallbackRoot := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFuncReturn(image.ArtifactStoreRootDir, storeRoot)
	patches.ApplyFuncReturn(image.ArtifactFallbackStoreRootDir, fallbackRoot)
	for _, root := range []string{workRoot, storeRoot, fallbackRoot} {
		for _, id := range []string{"rfs-interrupted", "rfs-unrelated"} {
			dir := filepath.Join(root, id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := deleteLocalArtifact("rfs-interrupted", ""); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{workRoot, storeRoot, fallbackRoot} {
		if _, err := os.Stat(filepath.Join(root, "rfs-interrupted")); !os.IsNotExist(err) {
			t.Fatalf("unreported residue remains under %s: %v", root, err)
		}
		if _, err := os.Stat(filepath.Join(root, "rfs-unrelated", "partial")); err != nil {
			t.Fatalf("unrelated artifact affected: %v", err)
		}
	}
	for _, id := range []string{"", "..", "../rfs-unrelated", "a/b", "/tmp/outside"} {
		if err := deleteLocalArtifact(id, ""); err == nil {
			t.Fatalf("accepted unsafe artifact ID %q", id)
		}
	}
	// Idempotent cleanup must not create directories to discover their paths.
	if err := deleteLocalArtifact("rfs-interrupted", ""); err != nil {
		t.Fatal(err)
	}
}

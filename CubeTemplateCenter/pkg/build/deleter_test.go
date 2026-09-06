// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests cover the DB-independent parts of artifact deletion: path
// traversal guards and the local-file removal logic. The DB claim/row-removal
// flow is exercised by integration tests against a real database, since TC's
// go.mod does not carry an embedded driver (glebarez/sqlite) today.

func TestDeleteLocalExt4RemovesManagedDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	artifactID := "rfs-del-001"
	artifactDir := filepath.Join(root, artifactID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ext4Path := filepath.Join(artifactDir, artifactID+".ext4")
	if err := os.WriteFile(ext4Path, []byte("fake ext4 data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}

	if err := deleteLocalExt4(ext4Path); err != nil {
		t.Fatalf("deleteLocalExt4: %v", err)
	}
	if _, err := os.Stat(artifactDir); !os.IsNotExist(err) {
		t.Fatalf("expected artifact dir removed, stat err=%v", err)
	}
}

func TestDeleteLocalExt4RefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	// A path OUTSIDE the managed root must be refused and NOT deleted.
	outsideDir := t.TempDir()
	evilPath := filepath.Join(outsideDir, "evil.ext4")
	if err := os.WriteFile(evilPath, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write evil file: %v", err)
	}

	err := deleteLocalExt4(evilPath)
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

	inside := filepath.Join(root, "rfs-abc", "sub")
	outside := t.TempDir()

	if !managedArtifactDir(inside) {
		t.Fatalf("expected %q recognized as managed", inside)
	}
	if managedArtifactDir(outside) {
		t.Fatalf("expected %q recognized as NOT managed", outside)
	}
	// Root itself is managed.
	if !managedArtifactDir(root) {
		t.Fatalf("expected root %q recognized as managed", root)
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

// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// Regression coverage for the P1 bug where READY-artifact reuse only ever
// checked os.Stat(Ext4Path), so an S3-backed artifact built by a sibling TC
// replica (no shared local disk) was always treated as "missing" and
// silently rebuilt instead of reused.

func TestArtifactDataExistsS3ObjectConfirmed(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-1",
		ArtifactURL: "https://s3.example.com/bucket/rfs-1",
		Ext4Path:    filepath.Join(t.TempDir(), "does-not-exist.ext4"), // local copy absent on this replica
	}
	statS3 := func() (bool, error) { return true, nil }

	reason, ok := artifactDataExists(artifact, statS3)
	if !ok {
		t.Fatalf("expected reuse when S3 HEAD confirms the object exists, reason=%q", reason)
	}
}

func TestArtifactDataExistsS3ObjectMissing(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-2",
		ArtifactURL: "https://s3.example.com/bucket/rfs-2",
		Ext4Path:    filepath.Join(t.TempDir(), "does-not-exist.ext4"),
	}
	statS3 := func() (bool, error) { return false, nil }

	if _, ok := artifactDataExists(artifact, statS3); ok {
		t.Fatal("expected no-reuse when S3 HEAD confirms the object is gone")
	}
}

func TestArtifactDataExistsS3ErrorFallsBackToLocalDisk(t *testing.T) {
	dir := t.TempDir()
	ext4Path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(ext4Path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-3",
		ArtifactURL: "https://s3.example.com/bucket/rfs-3",
		Ext4Path:    ext4Path,
	}
	statS3 := func() (bool, error) { return false, errors.New("network timeout") }

	if _, ok := artifactDataExists(artifact, statS3); !ok {
		t.Fatal("expected fallback to local disk when the S3 HEAD request itself errors")
	}
}

func TestArtifactDataExistsLocalOnlyArtifact(t *testing.T) {
	dir := t.TempDir()
	ext4Path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(ext4Path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID: "rfs-4",
		Ext4Path:   ext4Path,
		// ArtifactURL empty: local-only artifact, no S3 involved.
	}

	if _, ok := artifactDataExists(artifact, nil); !ok {
		t.Fatal("expected reuse for a local-only artifact whose ext4 file exists")
	}

	artifact.Ext4Path = filepath.Join(dir, "missing.ext4")
	if _, ok := artifactDataExists(artifact, nil); ok {
		t.Fatal("expected no-reuse for a local-only artifact whose ext4 file is missing")
	}
}

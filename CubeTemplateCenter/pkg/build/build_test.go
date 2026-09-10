// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
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

// fakeArtifactS3 is an in-memory artifactS3Ops for presign-guard tests.
type fakeArtifactS3 struct {
	objects map[string]bool
}

func (f *fakeArtifactS3) Stat(_ context.Context, artifactID string) (bool, error) {
	return f.objects[artifactID], nil
}

func (f *fakeArtifactS3) PresignedGetURL(_ context.Context, artifactID string) (string, error) {
	return "https://s3.example.com/bucket/" + artifactID + ".ext4?sig=x", nil
}

// Regression: reusing a legacy artifact that predates S3 (artifact_url empty,
// ext4 on local disk, nothing in the bucket) must NOT produce a presigned URL
// -- the signed URL 404s for cubelets and marks the row S3-backed, skipping
// the working local-file serving path.
func TestArtifactPresignedURLSkipsObjectsMissingFromBucket(t *testing.T) {
	s3 := &fakeArtifactS3{objects: map[string]bool{}}
	logger := log.G(context.Background())
	if got := artifactPresignedURL(context.Background(), true, s3, "rfs-legacy", logger); got != "" {
		t.Fatalf("expected empty URL for an object that was never uploaded, got %q", got)
	}
}

func TestArtifactPresignedURLSignsExistingObject(t *testing.T) {
	s3 := &fakeArtifactS3{objects: map[string]bool{"rfs-fresh": true}}
	logger := log.G(context.Background())
	got := artifactPresignedURL(context.Background(), true, s3, "rfs-fresh", logger)
	if !strings.Contains(got, "rfs-fresh.ext4") {
		t.Fatalf("expected presigned URL for an existing object, got %q", got)
	}
}

func TestArtifactPresignedURLDisabledWhenS3Off(t *testing.T) {
	s3 := &fakeArtifactS3{objects: map[string]bool{"rfs-1": true}}
	logger := log.G(context.Background())
	if got := artifactPresignedURL(context.Background(), false, s3, "rfs-1", logger); got != "" {
		t.Fatalf("expected empty URL when S3 is disabled, got %q", got)
	}
}

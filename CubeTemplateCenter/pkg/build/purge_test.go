// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

// fakeArtifactStore is an in-memory artifactUploader: the post-build stage is
// exercised end to end (upload -> reclaim) without a live bucket.
type fakeArtifactStore struct {
	backend    string
	uploadErr  error
	presignErr error
	uploads    []string
}

func (f *fakeArtifactStore) Upload(_ context.Context, artifactID, localPath, sha256 string) (string, error) {
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	f.uploads = append(f.uploads, artifactID)
	return "bucket/" + artifactID + ".ext4", nil
}

func (f *fakeArtifactStore) BackendName() string { return f.backend }

func (f *fakeArtifactStore) Stat(_ context.Context, artifactID string) (bool, error) {
	return true, nil
}

func (f *fakeArtifactStore) PresignedGetURL(_ context.Context, artifactID string) (string, error) {
	if f.presignErr != nil {
		return "", f.presignErr
	}
	return "https://s3.example.com/bucket/" + artifactID + ".ext4?sig=x", nil
}

// writeBuiltArtifact lays out <root>/<artifactID>/<artifactID>.ext4, the shape
// BuildExt4 leaves behind on a successful build.
func writeBuiltArtifact(t *testing.T, root, artifactID string) (ext4Path, dir string) {
	t.Helper()
	dir = filepath.Join(root, artifactID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	ext4Path = filepath.Join(dir, artifactID+".ext4")
	if err := os.WriteFile(ext4Path, []byte("fake ext4 data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	return ext4Path, dir
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to survive, stat err=%v", path, err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err=%v", path, err)
	}
}

// The upload stage must hand BUILT a download address, and must leave the
// durable-copy question to the caller: a successful Put is not by itself a
// reason to unlink anything.
func TestUploadBuiltArtifactReturnsDownloadURL(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	ext4Path, _ := writeBuiltArtifact(t, root, "rfs-upload-001")
	store := &fakeArtifactStore{backend: "s3"}

	url, err := uploadBuiltArtifact(t.Context(), store, "rfs-upload-001",
		&image.BuildResult{Ext4Path: ext4Path, SHA256: "sha"}, log.G(t.Context()))
	if err != nil {
		t.Fatalf("uploadBuiltArtifact: %v", err)
	}
	if url == "" {
		t.Fatal("expected a download URL for a successfully uploaded artifact")
	}
	mustExist(t, ext4Path)
}

// A failed Put is terminal: the build fails AND the local copy goes with it.
// The old behaviour fell back to the local disk, which reported BUILT for an
// artifact CubeMaster would never serve from that disk -- and left the ext4
// behind on every failed upload, the leak this change exists to close.
func TestUploadBuiltArtifactDiscardsLocalCopyOnFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	ext4Path, dir := writeBuiltArtifact(t, root, "rfs-upload-002")
	store := &fakeArtifactStore{backend: "s3", uploadErr: errors.New("minio: connection refused")}

	url, err := uploadBuiltArtifact(t.Context(), store, "rfs-upload-002",
		&image.BuildResult{Ext4Path: ext4Path, SHA256: "sha"}, log.G(t.Context()))
	if err == nil {
		t.Fatal("expected a failed upload to fail the build")
	}
	if url != "" {
		t.Fatalf("expected no download URL on failure, got %q", url)
	}
	mustBeGone(t, ext4Path)
	mustBeGone(t, dir)
}

// Reclaim after BUILT: only an s3-backed artifact with a recorded download
// address is disposable.
func TestPurgeBuiltArtifactAfterReport(t *testing.T) {
	const url = "https://s3.example.com/bucket/rfs-1.ext4?sig=x"
	cases := []struct {
		name        string
		uploaded    bool
		artifactURL string
		backend     string
		wantPurged  bool
	}{
		{"s3 upload with a download url", true, url, "s3", true},
		{"fs backend may own the very same directory", true, url, "fs", false},
		{"no upload means no durable copy", false, url, "s3", false},
		{"unresolved backend keeps the file", true, url, "", false},
		// storage_backend=s3 with an empty artifact_url is a 502 on the Master
		// download endpoint, so the bytes stay: they are the only copy left.
		{"no download url keeps the bytes", true, "", "s3", false},
		{"blank download url keeps the bytes", true, "   ", "s3", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
			ext4Path, _ := writeBuiltArtifact(t, root, "rfs-case-"+string(rune('a'+i)))

			purged := purgeBuiltArtifactAfterReport(ext4Path, tc.uploaded, tc.artifactURL, tc.backend, log.G(t.Context()))
			if purged != tc.wantPurged {
				t.Fatalf("purgeBuiltArtifactAfterReport(uploaded=%t url=%q backend=%q) = %t, want %t",
					tc.uploaded, tc.artifactURL, tc.backend, purged, tc.wantPurged)
			}
			if tc.wantPurged {
				mustBeGone(t, ext4Path)
			} else {
				mustExist(t, ext4Path)
			}
		})
	}
}

func TestPurgeLocalArtifactCopyRemovesFileAndEmptyDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	ext4Path, dir := writeBuiltArtifact(t, root, "rfs-purge-001")

	if !purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected the local copy to be reported as purged")
	}
	mustBeGone(t, ext4Path)
	mustBeGone(t, dir)
}

// A concurrent build's marker or a native-exporter prefetch dir can still be in
// the artifact directory. Only the file may be unlinked: RemoveAll there would
// delete another build's working data.
func TestPurgeLocalArtifactCopyKeepsNonEmptyDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	ext4Path, dir := writeBuiltArtifact(t, root, "rfs-purge-002")
	prefetch := filepath.Join(dir, "native-prefetch-sha256-abc")
	if err := os.WriteFile(prefetch, []byte("layer"), 0o644); err != nil {
		t.Fatalf("write prefetch file: %v", err)
	}

	if !purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected the local copy to be reported as purged")
	}
	mustBeGone(t, ext4Path)
	mustExist(t, prefetch)
}

func TestPurgeLocalArtifactCopyRefusesOutsideManagedRoots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	outside := t.TempDir()
	ext4Path, _ := writeBuiltArtifact(t, outside, "rfs-purge-003")

	if purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected a path outside the managed artifact roots to be refused")
	}
	mustExist(t, ext4Path)
}

// A symlinked artifact directory passes a lexical <root>/<artifactID> check but
// names a file outside the roots. Unlinking THROUGH it deletes that file, and TC
// runs as root, so the guard has to resolve symlinks before it approves a path.
func TestPurgeLocalArtifactCopyRefusesSymlinkedArtifactDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	outside := t.TempDir()
	outsideExt4, outsideDir := writeBuiltArtifact(t, outside, "rfs-purge-005")

	link := filepath.Join(root, "rfs-purge-005")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if purgeLocalArtifactCopy(filepath.Join(link, "rfs-purge-005.ext4"), log.G(t.Context())) {
		t.Fatal("expected a symlinked artifact dir pointing outside the roots to be refused")
	}
	mustExist(t, outsideExt4)
}

func TestPurgeLocalArtifactCopyMissingFileIsNoop(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	// A replica that never held the artifact (or a `tpl merge` that already
	// cleaned it) must be a silent no-op, not an error path.
	ext4Path := filepath.Join(root, "rfs-purge-004", "rfs-purge-004.ext4")
	if purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected no purge report for a file that is already gone")
	}
}

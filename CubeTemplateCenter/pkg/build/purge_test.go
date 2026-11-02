// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// The local ext4 stays the artifact's only copy unless the bucket holds it, so
// the predicate gating the post-build cleanup is pinned here: every condition
// that makes the local file disposable is required, and a missing one keeps it.

func TestShouldPurgeLocalArtifact(t *testing.T) {
	const url = "https://s3.example.com/bucket/rfs-1.ext4?sig=x"
	cases := []struct {
		name        string
		optIn       bool
		uploaded    bool
		artifactURL string
		backend     string
		want        bool
	}{
		{"opted in after a successful s3 upload", true, true, url, "s3", true},
		{"flag off keeps the download cache", false, true, url, "s3", false},
		{"failed upload keeps the only copy", true, false, url, "s3", false},
		{"fs backend may own the very same directory", true, true, url, "fs", false},
		{"unresolved backend keeps the file", true, true, url, "", false},
		{"no download url means master reads the file", true, true, "", "s3", false},
		{"blank download url means master reads the file", true, true, "   ", "s3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldPurgeLocalArtifact(tc.optIn, tc.uploaded, tc.artifactURL, tc.backend)
			if got != tc.want {
				t.Fatalf("shouldPurgeLocalArtifact(optIn=%t uploaded=%t url=%q backend=%q) = %t, want %t",
					tc.optIn, tc.uploaded, tc.artifactURL, tc.backend, got, tc.want)
			}
		})
	}
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

func TestPurgeLocalArtifactCopyRemovesFileAndEmptyDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	ext4Path, dir := writeBuiltArtifact(t, root, "rfs-purge-001")

	if !purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected the local copy to be reported as purged")
	}
	if _, err := os.Stat(ext4Path); !os.IsNotExist(err) {
		t.Fatalf("expected the ext4 to be unlinked, stat err=%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected the now-empty artifact dir to be removed, stat err=%v", err)
	}
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
	if _, err := os.Stat(ext4Path); !os.IsNotExist(err) {
		t.Fatalf("expected the ext4 to be unlinked, stat err=%v", err)
	}
	if _, err := os.Stat(prefetch); err != nil {
		t.Fatalf("expected the prefetch file to survive, stat err=%v", err)
	}
}

func TestPurgeLocalArtifactCopyRefusesOutsideManagedRoots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
	outside := t.TempDir()
	ext4Path, _ := writeBuiltArtifact(t, outside, "rfs-purge-003")

	if purgeLocalArtifactCopy(ext4Path, log.G(t.Context())) {
		t.Fatal("expected a path outside the managed artifact roots to be refused")
	}
	if _, err := os.Stat(ext4Path); err != nil {
		t.Fatalf("expected the outside file to be preserved, stat err=%v", err)
	}
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

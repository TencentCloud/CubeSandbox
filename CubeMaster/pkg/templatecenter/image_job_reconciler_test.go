// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeArtifactFile creates an empty file under a temp store root and
// points CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR at that root, satisfying both
// checks remoteBuildResultFromResultJSON performs on Ext4Path: it must exist
// on disk (os.Stat) AND live under one of the configured artifact store
// roots (RemoteBuildResult.Validate).
func writeFakeArtifactFile(t *testing.T, name string) string {
	t.Helper()
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	path := filepath.Join(storeRoot, name)
	if err := os.WriteFile(path, []byte("fake-ext4"), 0o644); err != nil {
		t.Fatalf("write fake artifact file: %v", err)
	}
	return path
}

// Regression test: remoteBuildResultFromResultJSON used to omit ArtifactURL
// from the decoded payload, so replaying a stuck BUILT job via the
// reconciler silently zeroed out (or overwrote) the S3/MinIO presigned URL
// that TC had reported in the original terminal callback.
func TestRemoteBuildResultFromResultJSONPreservesArtifactURL(t *testing.T) {
	ext4Path := writeFakeArtifactFile(t, "rootfs.ext4")
	payload := fmt.Sprintf(`{
		"artifact_id": "rfs-1",
		"template_spec_fingerprint": "fp-1",
		"source_image_digest": "sha256:abc",
		"ext4_path": %q,
		"ext4_sha256": "sha256:def",
		"ext4_size_bytes": 1024,
		"image_config_json": "{}",
		"master_node_ip": "http://master:8080",
		"artifact_url": "https://s3.example.com/bucket/rfs-1?X-Amz-Signature=abc",
		"cube_egress_ca_baked": true,
		"cube_egress_ca_fingerprint": "ca-fp",
		"cube_egress_ca_targets_written": 2
	}`, ext4Path)

	result, err := remoteBuildResultFromResultJSON(payload)
	if err != nil {
		t.Fatalf("remoteBuildResultFromResultJSON() error = %v", err)
	}
	want := "https://s3.example.com/bucket/rfs-1?X-Amz-Signature=abc"
	if result.ArtifactURL != want {
		t.Fatalf("ArtifactURL = %q, want %q", result.ArtifactURL, want)
	}
}

// When the original callback never had an S3 URL (local-disk-only build),
// replay must not fabricate one -- it stays empty, distinct from an error.
func TestRemoteBuildResultFromResultJSONEmptyArtifactURL(t *testing.T) {
	ext4Path := writeFakeArtifactFile(t, "rootfs.ext4")
	payload := fmt.Sprintf(`{
		"artifact_id": "rfs-2",
		"template_spec_fingerprint": "fp-2",
		"source_image_digest": "sha256:abc",
		"ext4_path": %q,
		"ext4_sha256": "sha256:def",
		"ext4_size_bytes": 1024,
		"master_node_ip": "http://master:8080"
	}`, ext4Path)

	result, err := remoteBuildResultFromResultJSON(payload)
	if err != nil {
		t.Fatalf("remoteBuildResultFromResultJSON() error = %v", err)
	}
	if result.ArtifactURL != "" {
		t.Fatalf("ArtifactURL = %q, want empty", result.ArtifactURL)
	}
}

// Replayed build results must not require the TC-owned ext4 on the Master's
// disk. Both S3 and TC-local artifacts are validated against the actual owner
// during distribution, not against a path on an unrelated Master.
func TestRemoteBuildResultFromResultJSONSkipsMasterLocalStat(t *testing.T) {
	storeRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
	missing := filepath.Join(storeRoot, "gone.ext4") // inside the store root, but never written

	mkPayload := func(artifactURL string) string {
		return fmt.Sprintf(`{
			"artifact_id": "rfs-3",
			"template_spec_fingerprint": "fp-3",
			"source_image_digest": "sha256:abc",
			"ext4_path": %q,
			"ext4_sha256": "sha256:def",
			"ext4_size_bytes": 1024,
			"master_node_ip": "http://master:8080",
			"artifact_url": %q
		}`, missing, artifactURL)
	}

	// S3-backed: replay succeeds despite the missing local file.
	result, err := remoteBuildResultFromResultJSON(mkPayload("https://s3.example.com/bucket/rfs-3?X-Amz-Signature=abc"))
	if err != nil {
		t.Fatalf("S3-backed replay must not require the local ext4: %v", err)
	}
	if result.ArtifactURL == "" {
		t.Fatal("ArtifactURL lost in replay")
	}

	result, err = remoteBuildResultFromResultJSON(mkPayload(""))
	if err != nil {
		t.Fatalf("TC-local replay must not require the ext4 on the Master: %v", err)
	}
	if result.Ext4Path != missing || result.MasterNodeIP != "http://master:8080" || result.ArtifactURL != "" {
		t.Fatalf("TC-local artifact locator lost in replay: %+v", result)
	}
}

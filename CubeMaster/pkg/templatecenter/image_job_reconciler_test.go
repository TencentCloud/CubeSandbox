// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
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
func TestRootfsArtifactDownloadTokenRejectsEmptyLegacyToken(t *testing.T) {
	if validRootfsArtifactDownloadToken(&models.RootfsArtifact{ArtifactID: "rfs-1"}, "") {
		t.Fatal("empty stored token must not authorize an anonymous download")
	}
}

func TestNormalizeRemoteBuildArtifactStore(t *testing.T) {
	t.Setenv("CUBE_S3_ENDPOINT", "minio.example")
	t.Setenv("CUBE_S3_BUCKET", "test-artifacts")
	t.Setenv("CUBE_S3_ACCESS_KEY_ID", "test-key")
	t.Setenv("CUBE_S3_SECRET_ACCESS_KEY", "test-secret")
	cases := []struct {
		name    string
		input   RemoteBuildResult
		want    string
		wantErr bool
	}{
		{"new s3 metadata", RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4", Bucket: "test-artifacts"}, "blobstore:s3:rfs-1.ext4", false},
		{"legacy s3 metadata without bucket", RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"}, "blobstore:s3:rfs-1.ext4", false},
		{"new tc fs metadata", RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "fs", ObjectKey: "rfs-1.ext4"}, "blobstore:fs:rfs-1.ext4", false},
		{"locator without columns", RemoteBuildResult{ArtifactID: "rfs-1", ArtifactURL: "blobstore:s3:rfs-1.ext4"}, "blobstore:s3:rfs-1.ext4", false},
		{"old tc with columns", RemoteBuildResult{ArtifactID: "rfs-1", ArtifactURL: "https://minio.example/test-artifacts/rfs-1.ext4?X-Amz-Signature=old", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"}, "blobstore:s3:rfs-1.ext4", false},
		{"old tc different bucket", RemoteBuildResult{ArtifactID: "rfs-1", ArtifactURL: "https://minio.example/other-artifacts/rfs-1.ext4?X-Amz-Signature=old", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"}, "", true},
		{"new tc different bucket", RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4", Bucket: "other-artifacts"}, "", true},
		{"local disk", RemoteBuildResult{ArtifactID: "rfs-1"}, "", false},
		{"conflicting locator", RemoteBuildResult{ArtifactID: "rfs-1", ArtifactURL: "blobstore:fs:rfs-1.ext4", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"}, "", true},
		{"invalid key", RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "../other"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.input
			err := NormalizeRemoteBuildArtifactStore(context.Background(), &result)
			if (err != nil) != tc.wantErr || (!tc.wantErr && result.ArtifactURL != tc.want) {
				t.Fatalf("normalized artifact_url=%q error=%v, want %q err=%v", result.ArtifactURL, err, tc.want, tc.wantErr)
			}
		})
	}
}

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

// An S3-backed result (artifact_url set) must replay even when the local
// ext4 is gone: the bucket object is the durable copy and cubelets pull from
// the presigned URL, so on a multi-replica / un-colocated deployment the
// replay may land on a master that never shared TC's disk. Local-disk
// results keep the stat guard.
func TestRemoteBuildResultFromResultJSONS3SkipsLocalStat(t *testing.T) {
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

	// S3-backed: replay succeeds despite the missing local file, including
	// when TC could not presign after upload and reported a durable locator.
	for _, artifactURL := range []string{
		"https://s3.example.com/bucket/rfs-3?X-Amz-Signature=abc",
		"blobstore:s3:templates/rfs-3.ext4",
	} {
		result, err := remoteBuildResultFromResultJSON(mkPayload(artifactURL))
		if err != nil {
			t.Fatalf("S3-backed replay must not require the local ext4: %v", err)
		}
		if result.ArtifactURL != artifactURL {
			t.Fatalf("ArtifactURL = %q, want %q", result.ArtifactURL, artifactURL)
		}
	}
	// New TC can report stable backend/key without any signed URL.
	metadataOnly := strings.Replace(mkPayload(""), `"artifact_url": ""`, `"storage_backend": "s3", "object_key": "rfs-3.ext4"`, 1)
	if got, err := remoteBuildResultFromResultJSON(metadataOnly); err != nil || got.StorageBackend != "s3" {
		t.Fatalf("metadata-only S3 replay failed: result=%+v err=%v", got, err)
	}

	// Local-disk: same missing file is still an error.
	if _, err := remoteBuildResultFromResultJSON(mkPayload("")); err == nil {
		t.Fatal("local-disk replay with a missing ext4 must fail")
	}
}

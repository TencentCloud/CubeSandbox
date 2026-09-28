// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"net/url"
	"testing"
)

// TestRewriteDownloadHostLeavesS3PresignedURLUntouched reproduces the
// production failure: distributeRootfsArtifact hands cubelets the raw S3
// presigned URL when TC uploaded the artifact to S3. Rewriting its host to
// CubeMaster's address invalidates the SigV4 signature and points at a path
// CubeMaster never routes, so the download 404s ("download status code
// 404" -> "ensure pmem file failed").
func TestRewriteDownloadHostLeavesS3PresignedURLUntouched(t *testing.T) {
	s3URL := "https://minio.internal:9000/bucket/rfs-78ec90fb-b3d03faf?" +
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE&" +
		"X-Amz-Date=20260907T000000Z&X-Amz-Expires=3600&" +
		"X-Amz-SignedHeaders=host&X-Amz-Signature=deadbeefcafebabe"

	got := rewriteDownloadHost(s3URL, "cubemaster.svc:8089")
	if got != s3URL {
		t.Fatalf("rewriteDownloadHost mangled a presigned S3 URL:\n got:  %s\n want: %s", got, s3URL)
	}
}

// TestRewriteDownloadHostRewritesCubeMasterRoute keeps the original behavior
// for CubeMaster's own download route: the recorded MasterNodeIP may not be
// reachable from this cubelet, so the host must still be swapped for the
// locally configured cubemaster_http_addr.
func TestRewriteDownloadHostRewritesCubeMasterRoute(t *testing.T) {
	raw := "http://10.0.0.5:8089/cube/template/rootfs_artifact/download?artifact_id=rfs-x&token=tok"
	got := rewriteDownloadHost(raw, "cubemaster.svc:8089")
	want := "http://cubemaster.svc:8089/cube/template/rootfs_artifact/download?artifact_id=rfs-x&token=tok"
	if got != want {
		t.Fatalf("rewriteDownloadHost = %q, want %q", got, want)
	}
}

// TestRewriteDownloadHostEmptyEndpointPassesThrough covers the case where
// cubemaster_http_addr is unset (e.g. MetaServerConfig missing), which must
// stay a plain passthrough rather than producing a broken URL.
func TestRewriteDownloadHostEmptyEndpointPassesThrough(t *testing.T) {
	raw := "https://minio.internal:9000/bucket/rfs-x?X-Amz-Signature=abc"
	if got := rewriteDownloadHost(raw, ""); got != raw {
		t.Fatalf("rewriteDownloadHost = %q, want unchanged %q", got, raw)
	}
}

func TestIsS3PresignedURLDetectsSigV4Signature(t *testing.T) {
	cases := map[string]bool{
		"https://s3.example.com/bucket/key?X-Amz-Signature=abc":   true,
		"https://s3.example.com/bucket/key?X-Amz-Algorithm=SHA":   false,
		"http://cubemaster:8089/cube/template/download?token=abc": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) error: %v", raw, err)
		}
		if got := isS3PresignedURL(u); got != want {
			t.Errorf("isS3PresignedURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

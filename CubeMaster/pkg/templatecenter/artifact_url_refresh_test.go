// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// stubPresign swaps the signing seam and restores it on cleanup.
func stubPresign(t *testing.T, fn func(ctx context.Context, artifactID string) (string, error)) {
	t.Helper()
	old := presignArtifactGetURL
	presignArtifactGetURL = fn
	t.Cleanup(func() { presignArtifactGetURL = old })
}

func TestS3ArtifactObjectKeyMatchesS3Store(t *testing.T) {
	// These derivations MUST stay byte-identical with
	// CubeTemplateCenter/pkg/s3store.ObjectKey or re-signed URLs point at
	// objects that do not exist.
	cases := []struct{ prefix, id, want string }{
		{"", "rfs-1", "rfs-1.ext4"},
		{"template-artifacts/", "rfs-1", "template-artifacts/rfs-1.ext4"},
		{"template-artifacts", "rfs-1", "template-artifacts/rfs-1.ext4"},
		{"  ", "rfs-1", "rfs-1.ext4"},
	}
	for _, tc := range cases {
		if got := s3ArtifactObjectKey(tc.prefix, tc.id); got != tc.want {
			t.Fatalf("s3ArtifactObjectKey(%q, %q) = %q, want %q", tc.prefix, tc.id, got, tc.want)
		}
	}
}

// A local-disk artifact (no stored URL) must yield "" so the caller falls
// back to the Master-served download endpoint; signing must not even be
// attempted.
func TestArtifactDownloadURLLocalArtifact(t *testing.T) {
	stubPresign(t, func(context.Context, string) (string, error) {
		t.Fatal("presign must not be called for a local-disk artifact")
		return "", nil
	})
	got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-1"})
	if got != "" {
		t.Fatalf("got %q, want empty for local-disk artifact", got)
	}
	if got := artifactDownloadURL(context.Background(), nil); got != "" {
		t.Fatalf("got %q, want empty for nil artifact", got)
	}
}

// An S3-backed artifact gets a FRESH presigned URL at the point of use, never
// the aging one stored at build time.
func TestArtifactDownloadURLResigns(t *testing.T) {
	stubPresign(t, func(_ context.Context, artifactID string) (string, error) {
		return "https://minio:9000/bucket/" + artifactID + ".ext4?X-Amz-Signature=fresh", nil
	})
	stored := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=stale"
	got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-9", ArtifactURL: stored})
	want := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=fresh"
	if got != want {
		t.Fatalf("got %q, want freshly signed %q", got, want)
	}
}

// When signing is unavailable (no credentials on this process, or a signing
// error), the stored URL is the graceful fallback -- degraded, not dead.
func TestArtifactDownloadURLFallsBackToStored(t *testing.T) {
	stored := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=stale"
	for _, err := range []error{errS3PresignNotConfigured, errors.New("boom")} {
		stubPresign(t, func(context.Context, string) (string, error) { return "", err })
		got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-9", ArtifactURL: stored})
		if got != stored {
			t.Fatalf("err=%v: got %q, want stored url %q", err, got, stored)
		}
	}
}

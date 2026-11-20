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
func stubPresign(t *testing.T, fn func(ctx context.Context, artifact *models.RootfsArtifact) (string, error)) {
	t.Helper()
	old := presignArtifactGetURL
	presignArtifactGetURL = fn
	t.Cleanup(func() { presignArtifactGetURL = old })
}

// A local-disk artifact (no stored URL) must yield "" so the caller falls
// back to the Master-served download endpoint; signing must not even be
// attempted.
func TestArtifactUsesObjectStore(t *testing.T) {
	if ArtifactUsesObjectStore(nil) {
		t.Fatal("nil")
	}
	if ArtifactUsesObjectStore(&models.RootfsArtifact{}) {
		t.Fatal("empty row")
	}
	if !ArtifactUsesObjectStore(&models.RootfsArtifact{StorageBackend: "fs"}) {
		t.Fatal("backend")
	}
	if !ArtifactUsesObjectStore(&models.RootfsArtifact{ArtifactURL: "https://s3/x"}) {
		t.Fatal("url")
	}
}

func TestArtifactDownloadURLLocalArtifact(t *testing.T) {
	stubPresign(t, func(context.Context, *models.RootfsArtifact) (string, error) {
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

func TestArtifactDownloadURLSignsMetadataOnlyRow(t *testing.T) {
	stubPresign(t, func(_ context.Context, artifact *models.RootfsArtifact) (string, error) {
		if artifact.StorageBackend != "s3" || artifact.ObjectKey != "rfs-1.ext4" {
			t.Fatalf("metadata not passed to signer: %+v", artifact)
		}
		return "https://minio.example/rfs-1.ext4?X-Amz-Signature=fresh", nil
	})
	artifact := &models.RootfsArtifact{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"}
	if got := artifactDownloadURL(context.Background(), artifact); got != "https://minio.example/rfs-1.ext4?X-Amz-Signature=fresh" {
		t.Fatalf("metadata-only artifact failed to sign: %q", got)
	}
}

func TestValidateArtifactStoreIdentityRejectsWrongBackendOrKey(t *testing.T) {
	t.Setenv("CUBE_S3_ENDPOINT", "minio.example")
	t.Setenv("CUBE_S3_BUCKET", "test-artifacts")
	t.Setenv("CUBE_S3_ACCESS_KEY_ID", "test-key")
	t.Setenv("CUBE_S3_SECRET_ACCESS_KEY", "test-secret")
	st := &artifactStore{backend: "s3"}
	for _, artifact := range []*models.RootfsArtifact{
		{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4", Bucket: "other-artifacts"},
		{ArtifactID: "rfs-1", ArtifactURL: "https://minio.example/other-artifacts/rfs-1.ext4?X-Amz-Signature=stale"},
		{ArtifactID: "rfs-1", StorageBackend: "fs", ObjectKey: "rfs-1.ext4"},
		{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "other.ext4"},
		{ArtifactID: "rfs-1", StorageBackend: "s3", ArtifactURL: "blobstore:s3:other.ext4"},
	} {
		if err := validateArtifactStoreIdentity(st, artifact); err == nil {
			t.Fatalf("wrong store identity accepted: %+v", artifact)
		}
	}
}

func TestArtifactDownloadURLKeepsLocator(t *testing.T) {
	stubPresign(t, func(context.Context, *models.RootfsArtifact) (string, error) {
		t.Fatal("presign must not be called for a locator")
		return "", nil
	})
	stored := "blobstore:fs:template-artifacts/rfs-1.ext4"
	got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-1", ArtifactURL: stored})
	if got != stored {
		t.Fatalf("got %q want locator", got)
	}
}

// An S3-backed artifact gets a FRESH presigned URL at the point of use, never
// the aging one stored at build time.
func TestArtifactDownloadURLResigns(t *testing.T) {
	stubPresign(t, func(_ context.Context, artifact *models.RootfsArtifact) (string, error) {
		return "https://minio:9000/bucket/" + artifact.ArtifactID + ".ext4?X-Amz-Signature=fresh", nil
	})
	stored := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=stale"
	got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-9", ArtifactURL: stored})
	want := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=fresh"
	if got != want {
		t.Fatalf("got %q, want freshly signed %q", got, want)
	}
}

// A historical presigned URL is not an authentication fallback. Re-signing
// must succeed, otherwise the handler returns an error rather than fetching
// a potentially expired URL (or one pointing at an untrusted host).
func TestArtifactDownloadURLRejectsStoredURLWhenSigningFails(t *testing.T) {
	stored := "https://minio:9000/bucket/rfs-9.ext4?X-Amz-Signature=stale"
	for _, signErr := range []error{errS3PresignNotConfigured, errors.New("boom"), nil} {
		stubPresign(t, func(context.Context, *models.RootfsArtifact) (string, error) { return "", signErr })
		got := artifactDownloadURL(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-9", ArtifactURL: stored})
		if got != "" {
			t.Fatalf("err=%v: got %q, must not use stored URL", signErr, got)
		}
	}
}

func TestArtifactStoreKeyStripsConfiguredPrefix(t *testing.T) {
	artifact := &models.RootfsArtifact{ArtifactID: "rfs-1", ObjectKey: "template-artifacts/rfs-1.ext4"}
	ctx := context.Background()
	got := artifactStoreKeyWithPrefix(ctx, artifact, "template-artifacts")
	if got != "rfs-1.ext4" {
		t.Fatalf("prefixed object_key: got %q want rfs-1.ext4", got)
	}
	artifact.ObjectKey = "rfs-1.ext4"
	got = artifactStoreKeyWithPrefix(ctx, artifact, "template-artifacts")
	if got != "rfs-1.ext4" {
		t.Fatalf("user-key object_key: got %q", got)
	}
	artifact.ObjectKey = "old-prefix/nested/rfs-1.ext4"
	got = artifactStoreKeyWithPrefix(ctx, artifact, "template-artifacts")
	if got != "rfs-1.ext4" {
		t.Fatalf("foreign prefix must fall back to derived key, got %q", got)
	}
	artifact.ObjectKey = ""
	got = artifactStoreKeyWithPrefix(ctx, artifact, "template-artifacts")
	if got != "rfs-1.ext4" {
		t.Fatalf("empty object_key: got %q", got)
	}
	if artifactStoreKey(ctx, nil) != "" {
		t.Fatal("nil artifact")
	}
	if userKeyFromStoredObjectKey("/template-artifacts/rfs-1.ext4", "template-artifacts") != "rfs-1.ext4" {
		t.Fatal("leading slash on stored key")
	}
}

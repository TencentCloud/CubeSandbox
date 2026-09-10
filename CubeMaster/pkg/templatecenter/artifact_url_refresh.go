// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// This file re-signs S3/MinIO presigned artifact download URLs at the point
// of use.
//
// WHY MASTER, WHY LAZY
// --------------------
// artifact_url is a presigned GET URL with a finite validity (7 days, the
// SigV4 maximum), but the artifact itself lives far longer. Every consumer of
// the URL sits on the CubeMaster/TC serving side -- the download endpoint now
// proxies S3 through a stable CubeMaster URL rather than handing the raw URL to
// cubelets -- so freshness is Master's concern, not CubeTemplateCenter's (TC
// only ever WRITES the URL at build time). Re-signing lazily at each use beats
// a periodic DB rewrite on every
// axis: no background sweep, no concurrent-write guarding, no row churn, and
// the handed-out URL always has its full lifetime ahead of it regardless of
// how old the row is.
//
// minio-go's PresignedGetObject signs locally (no network round trip), so
// this adds microseconds to a distribution, not an RPC.
//
// CREDENTIALS
// -----------
// The same deployment-wide CUBE_S3_* variables TC / cubelet / s3lvol read
// (one-click exports them from .one-click.env to every unit; the chart
// passes them via .Values.global.env). When they are absent here the stored
// URL is returned unchanged -- correct for local-disk artifacts (empty URL)
// and a graceful degradation for S3 ones (the stored URL keeps whatever
// validity it has left).

const (
	envS3Endpoint        = "CUBE_S3_ENDPOINT"
	envS3Bucket          = "CUBE_S3_BUCKET"
	envS3AccessKey       = "CUBE_S3_ACCESS_KEY_ID"
	legacyEnvS3AccessKey = "CUBE_S3_ACCESS_KEY"
	envS3SecretKey       = "CUBE_S3_SECRET_ACCESS_KEY"
	legacyEnvS3SecretKey = "CUBE_S3_SECRET_KEY"
	envS3Region          = "CUBE_S3_REGION"
	envS3UsePathStyle    = "CUBE_S3_USE_PATH_STYLE"
	envS3UseSSL          = "CUBE_S3_USE_SSL"
	envS3ArtifactPrefix  = "CUBE_S3_ARTIFACT_PREFIX"
)

// s3PresignExpiry matches s3store.DefaultPresignExpiry (7 days, the SigV4
// maximum). Kept as a constant rather than an env override because every
// signer in the deployment must agree on the object lifetime semantics.
const s3PresignExpiry = 7 * 24 * time.Hour

// s3ArtifactObjectKey derives the object key exactly as
// CubeTemplateCenter/pkg/s3store.ObjectKey does: [prefix/]artifactID.ext4.
// The two derivations MUST stay identical or re-signed URLs point at objects
// that do not exist.
func s3ArtifactObjectKey(prefix, artifactID string) string {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return artifactID + ".ext4"
	}
	return prefix + "/" + artifactID + ".ext4"
}

// s3Presigner is a lazily constructed minio client plus the config needed to
// derive object keys. Construction reads the process environment once; a
// config change requires a restart, consistent with every other env-based
// setting in this process.
type s3Presigner struct {
	client *minio.Client
	bucket string
	prefix string
}

var (
	s3PresignerOnce sync.Once
	s3PresignerInst *s3Presigner
)

func s3BoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// loadS3Presigner builds the shared signer from the environment, returning
// nil when the configuration is incomplete (S3 not in use here).
func loadS3Presigner() *s3Presigner {
	endpoint := strings.TrimSpace(os.Getenv(envS3Endpoint))
	bucket := strings.TrimSpace(os.Getenv(envS3Bucket))
	accessKey := strings.TrimSpace(os.Getenv(envS3AccessKey))
	if accessKey == "" {
		accessKey = strings.TrimSpace(os.Getenv(legacyEnvS3AccessKey))
	}
	secretKey := strings.TrimSpace(os.Getenv(envS3SecretKey))
	if secretKey == "" {
		secretKey = strings.TrimSpace(os.Getenv(legacyEnvS3SecretKey))
	}
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil
	}
	// Strip the scheme if present; minio-go takes host only (mirrors
	// s3store.NewClient).
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimRight(endpoint, "/")

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: s3BoolEnv(envS3UseSSL),
		Region: strings.TrimSpace(os.Getenv(envS3Region)),
	}
	if s3BoolEnv(envS3UsePathStyle) {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, opts)
	if err != nil {
		// A malformed endpoint must not disable the fallback: callers still
		// have the stored URL.
		return nil
	}
	return &s3Presigner{
		client: client,
		bucket: bucket,
		prefix: strings.TrimSpace(os.Getenv(envS3ArtifactPrefix)),
	}
}

// presignArtifactGetURL is the single signing seam, indirected so tests can
// substitute a fake without a live object store.
var presignArtifactGetURL = func(ctx context.Context, artifactID string) (string, error) {
	s3PresignerOnce.Do(func() {
		s3PresignerInst = loadS3Presigner()
	})
	if s3PresignerInst == nil {
		return "", errS3PresignNotConfigured
	}
	s := s3PresignerInst
	reqParams := make(url.Values)
	reqParams.Set("response-content-disposition", fmt.Sprintf("attachment; filename=\"%s.ext4\"", artifactID))
	u, err := s.client.PresignedGetObject(ctx, s.bucket, s3ArtifactObjectKey(s.prefix, artifactID), s3PresignExpiry, reqParams)
	if err != nil {
		return "", fmt.Errorf("presign get %s: %w", artifactID, err)
	}
	return u.String(), nil
}

var errS3PresignNotConfigured = fmt.Errorf("s3 presign not configured on cubemaster")

// statArtifactObjectInS3 checks whether the S3 object for artifactID exists.
// Returns (false, nil) only for a definitive "not found".
var statArtifactObjectInS3 = func(ctx context.Context, artifactID string) (bool, error) {
	s3PresignerOnce.Do(func() {
		s3PresignerInst = loadS3Presigner()
	})
	if s3PresignerInst == nil {
		return false, errS3PresignNotConfigured
	}
	s := s3PresignerInst
	_, err := s.client.StatObject(ctx, s.bucket, s3ArtifactObjectKey(s.prefix, artifactID), minio.StatObjectOptions{})
	if err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == "NoSuchKey" || code == "NotFound" {
			return false, nil
		}
		return false, fmt.Errorf("stat s3 object for artifact %s: %w", artifactID, err)
	}
	return true, nil
}

// uploadArtifactFileToS3 uploads filePath as the object of artifactID.
var uploadArtifactFileToS3 = func(ctx context.Context, artifactID, filePath string) error {
	s3PresignerOnce.Do(func() {
		s3PresignerInst = loadS3Presigner()
	})
	if s3PresignerInst == nil {
		return errS3PresignNotConfigured
	}
	s := s3PresignerInst
	_, err := s.client.FPutObject(ctx, s.bucket, s3ArtifactObjectKey(s.prefix, artifactID), filePath, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("upload artifact %s to s3 from %s: %w", artifactID, filePath, err)
	}
	return nil
}

// ArtifactDownloadURL exports artifactDownloadURL for the HTTP layer (the
// download redirect endpoint), which lives in a different package.
func ArtifactDownloadURL(ctx context.Context, artifact *models.RootfsArtifact) string {
	return artifactDownloadURL(ctx, artifact)
}

// artifactDownloadURL resolves the download URL to hand out for an artifact
// RIGHT NOW.
//
//   - Local-disk artifact (no stored URL): "" so the caller falls back to the
//     Master-served download endpoint (buildDownloadURL).
//   - S3-backed artifact and this process holds the S3 credentials: a FRESH
//     presigned URL with its full 7-day validity, never the aging one stored
//     at build time. This is what keeps redo / scale-out / re-distribution /
//     download-redirect working past the first week of an artifact's life.
//   - S3-backed but signing is unavailable (no credentials, signing error):
//     the stored URL, which may still be within its validity. Degraded, not
//     dead.
func artifactDownloadURL(ctx context.Context, artifact *models.RootfsArtifact) string {
	if artifact == nil {
		return ""
	}
	stored := strings.TrimSpace(artifact.ArtifactURL)
	if stored == "" {
		return ""
	}
	fresh, err := presignArtifactGetURL(ctx, artifact.ArtifactID)
	if err != nil {
		if err != errS3PresignNotConfigured {
			log.G(ctx).Warnf("re-sign artifact url fail, using stored url: artifact_id=%s err=%v", artifact.ArtifactID, err)
		}
		return stored
	}
	return fresh
}

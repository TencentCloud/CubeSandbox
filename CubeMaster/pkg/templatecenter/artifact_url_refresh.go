// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
	fsdriver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/fs"
	s3driver "github.com/tencentcloud/CubeSandbox/pkgs/blobstore/driver/s3"
)

// New artifacts persist stable backend/key metadata and a blobstore locator,
// never a presigned GET URL. Historical rows may still contain a presigned URL;
// Master resolves the configured object and signs a fresh URL at the point of
// use. The download endpoint can also proxy the object through Master. This
// avoids DB rewrites as signatures expire and keeps signing credentials out of
// the artifact row.
//
// Signing requires the configured artifact store and its credentials. When
// unavailable, do not return the historical URL: it may be expired or refer
// to a different store. Local-disk artifacts have no object-store URL and
// continue through the existing Master download route.

// s3PresignExpiry matches s3store.DefaultPresignExpiry (7 days, the SigV4
// maximum). Kept as a constant rather than an env override because every
// signer in the deployment must agree on the object lifetime semantics.
const s3PresignExpiry = 7 * 24 * time.Hour

type artifactStore struct {
	store   blobstore.Store
	backend string
}

var (
	artifactStoreOnce    sync.Once
	artifactStoreInst    *artifactStore
	artifactStoreOpenErr error
)

func artifactUserKey(artifactID string) string {
	return blobstore.ArtifactExt4Key("", artifactID)
}

func sharedArtifactStore() *artifactStore {
	artifactStoreOnce.Do(func() {
		artifactStoreInst = loadArtifactStore()
	})
	return artifactStoreInst
}

// loadArtifactStore builds the shared artifact store from the environment,
// returning nil when the configuration is incomplete (S3 not in use here).
func loadArtifactStore() *artifactStore {
	backend := configenv.ArtifactStoreBackend()
	prefix := configenv.EnvOr(configenv.EnvS3ArtifactPrefix)
	if backend == "fs" {
		root := configenv.ResolveArtifactFSRoot()
		st, err := blobstore.Open(context.Background(), fsdriver.Options{Root: root}.Config(root, prefix))
		if err != nil {
			noteArtifactStoreErr("open", backend, err)
			return nil
		}
		if err := st.Prepare(context.Background()); err != nil {
			_ = st.Close()
			noteArtifactStoreErr("prepare", backend, err)
			return nil
		}
		return &artifactStore{store: st, backend: backend}
	}

	s3 := configenv.ParseArtifactS3()
	if !s3.Enabled {
		return nil
	}
	useSSL := s3.UseSSL
	st, err := blobstore.Open(context.Background(), s3driver.Options{
		Endpoint:           s3.Endpoint,
		AccessKeyID:        s3.AccessKey,
		SecretAccessKey:    s3.SecretKey,
		Bucket:             s3.Bucket,
		Region:             s3.Region,
		PathStyle:          s3.UsePathStyle,
		UseSSL:             &useSSL,
		PresignExpiry:      s3PresignExpiry,
		AttachmentBasename: true,
	}.Config(prefix))
	if err != nil {
		noteArtifactStoreErr("open", backend, err)
		return nil
	}
	return &artifactStore{store: st, backend: backend}
}

func noteArtifactStoreErr(phase, backend string, err error) {
	artifactStoreOpenErr = err
	log.G(context.Background()).Warnf("artifact store %s failed backend=%s: %v", phase, backend, err)
}

// InitArtifactStore logs the selected backend, records it on the shared
// artifact volume, and fails fast when backend=fs cannot be opened.
func InitArtifactStore(ctx context.Context) error {
	backend := configenv.ArtifactStoreBackend()
	if err := blobstore.AnnounceIfFS(configenv.ResolveArtifactFSRoot(), backend); err != nil {
		return err
	}
	inst := sharedArtifactStore()
	if backend == "fs" && inst == nil {
		if artifactStoreOpenErr != nil {
			return fmt.Errorf("CUBE_ARTIFACT_STORE_BACKEND=fs but the artifact store failed to open: %w", artifactStoreOpenErr)
		}
		return fmt.Errorf("CUBE_ARTIFACT_STORE_BACKEND=fs but the artifact store failed to open")
	}
	if inst != nil {
		log.G(ctx).Infof("storage backend selected backend=%s reason=explicit", backend)
		return nil
	}
	if !configenv.ParseArtifactS3().Enabled {
		log.G(ctx).Warnf("storage backend degraded requested=s3 effective=local-disk reason=incomplete CUBE_S3_* credentials")
		return nil
	}
	if artifactStoreOpenErr != nil {
		log.G(ctx).Warnf("storage backend degraded requested=s3 effective=local-disk reason=%v", artifactStoreOpenErr)
	}
	return nil
}

func artifactStoreBucket() string {
	if st := sharedArtifactStore(); st != nil && st.backend == "s3" {
		return configenv.ParseArtifactS3().Bucket
	}
	return ""
}

func artifactStoreColumns(artifactID string) (backend, objectKey string) {
	st := sharedArtifactStore()
	if st == nil {
		return "", ""
	}
	return st.backend, blobstore.ArtifactExt4Key(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), artifactID)
}

// presignArtifactGetURLWithTTL is the signing seam, indirected so tests can
// substitute a fake without a live object store.
var presignArtifactGetURLWithTTL = func(ctx context.Context, artifact *models.RootfsArtifact, ttl time.Duration) (string, error) {
	if artifact == nil {
		return "", errS3PresignNotConfigured
	}
	st := sharedArtifactStore()
	if st == nil {
		return "", errS3PresignNotConfigured
	}
	if err := validateArtifactStoreIdentity(st, artifact); err != nil {
		return "", err
	}
	u, err := st.store.SignedGetURL(ctx, artifactStoreKey(ctx, artifact), ttl)
	if err != nil {
		if errors.Is(err, blobstore.ErrUnsupported) {
			return "", nil
		}
		return "", fmt.Errorf("presign get %s: %w", artifact.ArtifactID, err)
	}
	if blobstore.IsObjectLocator(u) {
		return "", nil
	}
	return u, nil
}

var presignArtifactGetURL = func(ctx context.Context, artifact *models.RootfsArtifact) (string, error) {
	return presignArtifactGetURLWithTTL(ctx, artifact, s3PresignExpiry)
}

var errS3PresignNotConfigured = fmt.Errorf("s3 presign not configured on cubemaster")

// statArtifactObjectInS3 checks whether the object for this artifact row exists.
// Returns (false, nil) only for a definitive "not found".
var statArtifactObjectInS3 = func(ctx context.Context, artifact *models.RootfsArtifact) (bool, error) {
	if artifact == nil {
		return false, errS3PresignNotConfigured
	}
	st := sharedArtifactStore()
	if st == nil {
		return false, errS3PresignNotConfigured
	}
	if err := validateArtifactStoreIdentity(st, artifact); err != nil {
		return false, err
	}
	_, err := st.store.Stat(ctx, artifactStoreKey(ctx, artifact))
	if err != nil {
		if blobstore.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat s3 object for artifact %s: %w", artifact.ArtifactID, err)
	}
	return true, nil
}

// uploadArtifactFileToS3 uploads filePath as the object of artifactID.
var uploadArtifactFileToS3 = func(ctx context.Context, artifactID, filePath string) error {
	store := sharedArtifactStore()
	if store == nil {
		return errS3PresignNotConfigured
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("upload artifact %s to s3 from %s: %w", artifactID, filePath, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = store.store.Put(ctx, artifactUserKey(artifactID), f, blobstore.PutOptions{
		ContentType: "application/octet-stream",
		Size:        st.Size(),
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

// ArtifactUsesObjectStore reports whether the durable copy lives in blobstore.
func ArtifactUsesObjectStore(artifact *models.RootfsArtifact) bool {
	if artifact == nil {
		return false
	}
	if strings.TrimSpace(artifact.StorageBackend) != "" || strings.TrimSpace(artifact.ObjectKey) != "" {
		return true
	}
	stored := strings.TrimSpace(artifact.ArtifactURL)
	// New rows use a stable blobstore locator; historical rows may still carry
	// an expiring presigned URL. Keep both readable during the migration window.
	return blobstore.IsObjectLocator(stored) || stored != ""
}

// OpenArtifactObject reads the artifact from the configured blob backend.
func OpenArtifactObject(ctx context.Context, artifact *models.RootfsArtifact) (*blobstore.Object, error) {
	st := sharedArtifactStore()
	if st == nil {
		return nil, errS3PresignNotConfigured
	}
	if err := validateArtifactStoreIdentity(st, artifact); err != nil {
		return nil, err
	}
	return st.store.Get(ctx, artifactStoreKey(ctx, artifact), blobstore.GetOptions{})
}

func validateArtifactStoreIdentity(st *artifactStore, artifact *models.RootfsArtifact) error {
	if st == nil || artifact == nil {
		return errS3PresignNotConfigured
	}
	if artifact.Bucket != "" {
		if st.backend != "s3" || artifact.Bucket != configenv.ParseArtifactS3().Bucket {
			return fmt.Errorf("artifact %s bucket does not match configured S3 bucket", artifact.ArtifactID)
		}
	} else if st.backend == "s3" && strings.HasPrefix(artifact.ArtifactURL, "http") {
		key := strings.TrimSpace(artifact.ObjectKey)
		if key == "" {
			key = blobstore.ArtifactExt4Key(artifactStorePrefix(), artifact.ArtifactID)
		}
		if err := validateHistoricalArtifactURL(artifact.ArtifactURL, key); err != nil {
			return err
		}
	}
	backend := strings.TrimSpace(artifact.StorageBackend)
	if backend != "" && backend != st.backend {
		return fmt.Errorf("artifact %s uses backend %q but this CubeMaster is configured for %q", artifact.ArtifactID, backend, st.backend)
	}
	if key := strings.TrimSpace(artifact.ObjectKey); key != "" {
		// Never silently redirect a stable key to artifactID.ext4 under a
		// different prefix. Without a store ID, the configured namespace is
		// the only safe identity check available for new metadata rows.
		if want := blobstore.ArtifactExt4Key(artifactStorePrefix(), artifact.ArtifactID); key != want {
			return fmt.Errorf("artifact %s object_key %q does not match configured store key %q", artifact.ArtifactID, key, want)
		}
	}
	if locator := strings.TrimSpace(artifact.ArtifactURL); blobstore.IsObjectLocator(locator) {
		locatorBackend, locatorKey, err := blobstore.ParseObjectLocator(locator)
		if err != nil {
			return fmt.Errorf("artifact %s has invalid blobstore locator: %w", artifact.ArtifactID, err)
		}
		if locatorBackend != st.backend {
			return fmt.Errorf("artifact %s locator uses backend %q but this CubeMaster is configured for %q", artifact.ArtifactID, locatorBackend, st.backend)
		}
		if want := blobstore.ArtifactExt4Key(artifactStorePrefix(), artifact.ArtifactID); locatorKey != want {
			return fmt.Errorf("artifact %s locator key %q does not match configured store key %q", artifact.ArtifactID, locatorKey, want)
		}
		if backend != "" && backend != locatorBackend {
			return fmt.Errorf("artifact %s backend %q conflicts with locator backend %q", artifact.ArtifactID, backend, locatorBackend)
		}
		if key := strings.TrimSpace(artifact.ObjectKey); key != "" && key != locatorKey {
			return fmt.Errorf("artifact %s object_key conflicts with blobstore locator", artifact.ArtifactID)
		}
	}
	return nil
}

func artifactStoreKey(ctx context.Context, artifact *models.RootfsArtifact) string {
	if artifact == nil {
		return ""
	}
	if strings.TrimSpace(artifact.ObjectKey) == "" && blobstore.IsObjectLocator(artifact.ArtifactURL) {
		_, key, err := blobstore.ParseObjectLocator(artifact.ArtifactURL)
		if err == nil {
			copy := *artifact
			copy.ObjectKey = key
			artifact = &copy
		}
	}
	return artifactStoreKeyWithPrefix(ctx, artifact, artifactStorePrefix())
}

func artifactStorePrefix() string {
	return strings.Trim(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), "/")
}

func artifactStoreKeyWithPrefix(ctx context.Context, artifact *models.RootfsArtifact, prefix string) string {
	fallback := artifactUserKey(artifact.ArtifactID)
	stored := strings.TrimSpace(artifact.ObjectKey)
	if stored == "" {
		return fallback
	}
	key := userKeyFromStoredObjectKey(stored, prefix)
	if prefix != "" && strings.Contains(key, "/") {
		log.G(ctx).Warnf("artifact object_key %q does not match store prefix %q; using derived key %s", stored, prefix, fallback)
		return fallback
	}
	if blobstore.ValidateKey(key) != nil {
		return fallback
	}
	return key
}

func userKeyFromStoredObjectKey(stored, prefix string) string {
	stored = strings.Trim(strings.TrimSpace(stored), "/")
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		if stored == prefix {
			return ""
		}
		if p := prefix + "/"; strings.HasPrefix(stored, p) {
			return strings.TrimPrefix(stored, p)
		}
	}
	return stored
}

// artifactDownloadURL resolves the download URL to hand out for an artifact
// RIGHT NOW.
//
//   - Local-disk artifact (no stored URL or object metadata): "" so the caller
//     falls back to the Master-served download endpoint (buildDownloadURL).
//   - Blobstore artifact: a stable blobstore locator is returned for in-process
//     streaming; URL-producing callers sign it afresh from backend/key.
//   - Historical S3 row with a stored presigned URL: CubeMaster re-signs
//     from its configured store. It never uses an expired/unknown-host URL
//     as a fallback when signing is unavailable.
func artifactDownloadURL(ctx context.Context, artifact *models.RootfsArtifact) string {
	if artifact == nil {
		return ""
	}
	stored := strings.TrimSpace(artifact.ArtifactURL)
	if blobstore.IsObjectLocator(stored) {
		return stored
	}
	if stored == "" && !ArtifactUsesObjectStore(artifact) {
		return ""
	}
	fresh, err := presignArtifactGetURL(ctx, artifact)
	if err != nil {
		log.G(ctx).Warnf("sign artifact url from configured store failed: artifact_id=%s err=%v", artifact.ArtifactID, err)
		return ""
	}
	return fresh
}

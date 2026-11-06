// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// This file owns CubeMaster's artifact blobstore client and signing seams.
//
// New Cubelets obtain a short-lived ticket immediately before each attempt.
// For S3/MinIO, presignArtifactGetURLWithTTL creates a fresh URL whose lifetime
// is the configured ticket TTL; bytes then flow object store -> Cubelet and
// never through CubeMaster. The seven-day ArtifactURL persisted by older
// builders is retained only for upgrade compatibility and legacy endpoints.
//
// minio-go's PresignedGetObject signs locally (no network round trip), so
// this adds microseconds to a distribution, not an RPC.
//
// CREDENTIALS
// -----------
// CubeMaster needs the same deployment-wide CUBE_S3_* configuration as TC in
// order to issue S3 tickets. The ticket path fails closed when signing is not
// configured; it never falls back to ArtifactURL or Master byte proxying.

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
		NodeEndpoint:       s3.NodeEndpoint,
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

func artifactStoreColumns(artifactID string) (backend, objectKey string) {
	st := sharedArtifactStore()
	if st == nil {
		return "", ""
	}
	return st.backend, blobstore.ArtifactExt4Key(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), artifactID)
}

func artifactS3StorageRef() (string, error) {
	cfg := configenv.ParseArtifactS3()
	if !cfg.Enabled {
		return "", errS3PresignNotConfigured
	}
	endpoint := strings.TrimSpace(cfg.NodeEndpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(cfg.Endpoint)
	}
	u := &url.URL{
		Scheme: "s3",
		Host:   cfg.Bucket,
		Path:   "/" + strings.Trim(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), "/"),
	}
	query := u.Query()
	query.Set("endpoint", endpoint)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// presignArtifactGetURLWithTTL creates a fresh, bounded capability. Download
// tickets use this directly and never fall back to a URL persisted at build
// time.
var presignArtifactGetURLWithTTL = func(ctx context.Context, artifact *models.RootfsArtifact, ttl time.Duration) (string, error) {
	if artifact == nil {
		return "", errS3PresignNotConfigured
	}
	st := sharedArtifactStore()
	if st == nil {
		return "", errS3PresignNotConfigured
	}
	key, err := artifactStoreKey(ctx, artifact)
	if err != nil {
		return "", err
	}
	u, err := st.store.SignedGetURL(ctx, key, ttl)
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

// presignArtifactGetURL is retained for migration and legacy download paths.
// New Cubelet distribution always uses the explicit-TTL function above.
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
	key, err := artifactStoreKey(ctx, artifact)
	if err != nil {
		return false, err
	}
	_, err = st.store.Stat(ctx, key)
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
	if strings.TrimSpace(artifact.StorageBackend) != "" {
		return true
	}
	u := strings.TrimSpace(artifact.ArtifactURL)
	return u != ""
}

// OpenArtifactObject reads the artifact from the configured blob backend.
func OpenArtifactObject(ctx context.Context, artifact *models.RootfsArtifact) (*blobstore.Object, error) {
	st := sharedArtifactStore()
	if st == nil {
		return nil, errS3PresignNotConfigured
	}
	key, err := artifactStoreKey(ctx, artifact)
	if err != nil {
		return nil, err
	}
	return st.store.Get(ctx, key, blobstore.GetOptions{})
}

func artifactStoreKey(ctx context.Context, artifact *models.RootfsArtifact) (string, error) {
	if artifact == nil {
		return "", errors.New("artifact is nil")
	}
	return artifactStoreKeyWithPrefix(ctx, artifact, artifactStorePrefix())
}

func artifactStorePrefix() string {
	return strings.Trim(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), "/")
}

func artifactStoreKeyWithPrefix(ctx context.Context, artifact *models.RootfsArtifact, prefix string) (string, error) {
	fallback := artifactUserKey(artifact.ArtifactID)
	stored := strings.TrimSpace(artifact.ObjectKey)
	if stored == "" {
		return fallback, validateArtifactS3Locator(artifact)
	}
	key := userKeyFromStoredObjectKey(stored, prefix)
	if prefix != "" && strings.Contains(key, "/") {
		return "", fmt.Errorf("artifact object_key %q does not match configured prefix %q", stored, prefix)
	}
	if err := blobstore.ValidateKey(key); err != nil {
		return "", fmt.Errorf("artifact object_key %q is invalid: %w", stored, err)
	}
	if err := validateArtifactS3Locator(artifact); err != nil {
		return "", err
	}
	return key, nil
}

func validateArtifactS3Locator(artifact *models.RootfsArtifact) error {
	if artifact == nil || strings.TrimSpace(artifact.StorageRef) == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(artifact.StorageRef))
	if err != nil || !strings.EqualFold(u.Scheme, "s3") || strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("artifact %s has invalid S3 storage_ref", artifact.ArtifactID)
	}
	cfg := configenv.ParseArtifactS3()
	if !strings.EqualFold(strings.TrimSpace(u.Host), strings.TrimSpace(cfg.Bucket)) {
		return fmt.Errorf("artifact %s belongs to S3 bucket %q, configured bucket is %q",
			artifact.ArtifactID, u.Host, cfg.Bucket)
	}
	recordedEndpoint := strings.TrimRight(strings.ToLower(strings.TrimSpace(u.Query().Get("endpoint"))), "/")
	configuredEndpoint := strings.TrimRight(strings.ToLower(strings.TrimSpace(cfg.NodeEndpoint)), "/")
	if configuredEndpoint == "" {
		configuredEndpoint = strings.TrimRight(strings.ToLower(strings.TrimSpace(cfg.Endpoint)), "/")
	}
	if recordedEndpoint != "" && recordedEndpoint != configuredEndpoint {
		return fmt.Errorf("artifact %s belongs to S3 endpoint %q, configured endpoint is %q",
			artifact.ArtifactID, recordedEndpoint, configuredEndpoint)
	}
	return nil
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

// artifactDownloadURL supports the pre-ticket compatibility endpoint only.
// New distribution must call presignArtifactGetURLWithTTL through
// buildArtifactDownloadTicket and must not use this fallback behavior.
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
	if blobstore.IsObjectLocator(stored) {
		return stored
	}
	fresh, err := presignArtifactGetURL(ctx, artifact)
	if err != nil {
		if err != errS3PresignNotConfigured {
			log.G(ctx).Warnf("re-sign artifact url fail, using stored url: artifact_id=%s err=%v", artifact.ArtifactID, err)
		}
		return stored
	}
	if fresh != "" {
		return fresh
	}
	return stored
}

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

func artifactStoreColumns(artifactID string) (backend, objectKey string) {
	st := sharedArtifactStore()
	if st == nil {
		return "", ""
	}
	return st.backend, blobstore.ArtifactExt4Key(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), artifactID)
}

// presignArtifactGetURL is the single signing seam, indirected so tests can
// substitute a fake without a live object store.
var presignArtifactGetURL = func(ctx context.Context, artifactID string) (string, error) {
	st := sharedArtifactStore()
	if st == nil {
		return "", errS3PresignNotConfigured
	}
	u, err := st.store.SignedGetURL(ctx, artifactUserKey(artifactID), s3PresignExpiry)
	if err != nil {
		if errors.Is(err, blobstore.ErrUnsupported) {
			return "", nil
		}
		return "", fmt.Errorf("presign get %s: %w", artifactID, err)
	}
	if blobstore.IsObjectLocator(u) {
		return "", nil
	}
	return u, nil
}

var errS3PresignNotConfigured = fmt.Errorf("s3 presign not configured on cubemaster")

// statArtifactObjectInS3 checks whether the S3 object for artifactID exists.
// Returns (false, nil) only for a definitive "not found".
var statArtifactObjectInS3 = func(ctx context.Context, artifactID string) (bool, error) {
	st := sharedArtifactStore()
	if st == nil {
		return false, errS3PresignNotConfigured
	}
	_, err := st.store.Stat(ctx, artifactUserKey(artifactID))
	if err != nil {
		if blobstore.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat s3 object for artifact %s: %w", artifactID, err)
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
	return st.store.Get(ctx, artifactStoreKey(artifact), blobstore.GetOptions{})
}

func artifactStoreKey(artifact *models.RootfsArtifact) string {
	key := strings.TrimSpace(artifact.ObjectKey)
	if key == "" {
		return artifactUserKey(artifact.ArtifactID)
	}
	return blobstore.BaseName(key)
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
	if blobstore.IsObjectLocator(stored) {
		return stored
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

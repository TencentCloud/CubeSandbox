// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func TestArtifactDataExistsS3ObjectConfirmed(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-1",
		ArtifactURL: "https://s3.example.com/bucket/rfs-1",
		RootfsPath:  filepath.Join(t.TempDir(), "does-not-exist.ext4"), // local copy absent on this replica
	}
	statS3 := func() (bool, error) { return true, nil }

	reason, ok := artifactDataExists(artifact, statS3)
	if !ok {
		t.Fatalf("expected reuse when S3 HEAD confirms the object exists, reason=%q", reason)
	}
}

func TestArtifactDataExistsS3ObjectMissing(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-2",
		ArtifactURL: "https://s3.example.com/bucket/rfs-2",
		RootfsPath:  filepath.Join(t.TempDir(), "does-not-exist.ext4"),
	}
	statS3 := func() (bool, error) { return false, nil }

	if _, ok := artifactDataExists(artifact, statS3); ok {
		t.Fatal("expected no-reuse when S3 HEAD confirms the object is gone")
	}
}

func TestArtifactDataExistsS3ErrorFallsBackToLocalDisk(t *testing.T) {
	dir := t.TempDir()
	ext4Path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(ext4Path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-3",
		ArtifactURL: "https://s3.example.com/bucket/rfs-3",
		RootfsPath:  ext4Path,
	}
	statS3 := func() (bool, error) { return false, errors.New("network timeout") }

	if _, ok := artifactDataExists(artifact, statS3); !ok {
		t.Fatal("expected fallback to local disk when the S3 HEAD request itself errors")
	}
}

func TestArtifactDataExistsBackendWithoutURL(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:     "rfs-backend",
		StorageBackend: "fs",
		RootfsPath:     filepath.Join(t.TempDir(), "does-not-exist.ext4"),
	}
	statS3 := func() (bool, error) { return true, nil }
	if _, ok := artifactDataExists(artifact, statS3); !ok {
		t.Fatal("expected reuse when storage_backend is set even without artifact_url")
	}
}

func TestArtifactDataExistsLocalOnlyArtifact(t *testing.T) {
	dir := t.TempDir()
	ext4Path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(ext4Path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID: "rfs-4",
		RootfsPath: ext4Path,
		// ArtifactURL empty: local-only artifact, no S3 involved.
	}

	if _, ok := artifactDataExists(artifact, nil); !ok {
		t.Fatal("expected reuse for a local-only artifact whose ext4 file exists")
	}

	artifact.RootfsPath = filepath.Join(dir, "missing.ext4")
	if _, ok := artifactDataExists(artifact, nil); ok {
		t.Fatal("expected no-reuse for a local-only artifact whose ext4 file is missing")
	}
}

// fakeArtifactS3 is an in-memory artifactS3Ops for presign-guard tests.
type fakeArtifactS3 struct {
	objects   map[string]bool
	statCalls int
	signErr   error
}

func (f *fakeArtifactS3) Stat(_ context.Context, artifactID string) (bool, error) {
	f.statCalls++
	return f.objects[artifactID], nil
}

func (f *fakeArtifactS3) PresignedGetURL(_ context.Context, artifactID string) (string, error) {
	return "https://s3.example.com/bucket/" + artifactID + "?sig=x", f.signErr
}

func (f *fakeArtifactS3) BackendName() string { return "s3" }

// Regression: reusing a legacy artifact that predates S3 (artifact_url empty,
// ext4 on local disk, nothing in the bucket) must NOT produce a presigned URL
// -- the signed URL 404s for cubelets and marks the row S3-backed, skipping
// the working local-file serving path.
func TestFindStoredArtifactSkipsObjectsMissingFromBucket(t *testing.T) {
	s3 := &fakeArtifactS3{objects: map[string]bool{}}
	logger := log.G(context.Background())
	if got := findStoredArtifact(context.Background(), s3, "rfs-legacy", logger); got != (artifactStoreLocation{}) {
		t.Fatalf("expected no location for an object that was never uploaded, got %+v", got)
	}
}

func TestFindStoredArtifactUsesExactObjectKey(t *testing.T) {
	key := "artifacts/custom/rootfs.data"
	s3 := &fakeArtifactS3{objects: map[string]bool{key: true}}
	logger := log.G(context.Background())
	got := findStoredArtifact(context.Background(), s3, key, logger)
	if got.ObjectKey != key || got.Backend != "s3" || !strings.Contains(got.URL, key+"?sig=x") || s3.statCalls != 1 {
		t.Fatalf("location=%+v stat calls=%d", got, s3.statCalls)
	}
}

func TestStoredArtifactLocationPreservesIdentityWhenSigningFails(t *testing.T) {
	for _, err := range []error{errors.New("signing failed"), blobstore.ErrUnsupported} {
		store := &fakeArtifactS3{signErr: err}
		got := storedArtifactLocation(context.Background(), store, "artifacts/object.data", log.G(context.Background()))
		if got.URL != "" || got.ObjectKey != "artifacts/object.data" || got.Backend != "s3" || store.statCalls != 0 {
			t.Fatalf("location=%+v stat calls=%d", got, store.statCalls)
		}
	}
}

func TestReportExistingArtifactKeepsPublishedKey(t *testing.T) {
	ctx := context.Background()
	store, err := s3store.NewClient(s3store.Config{Driver: "fs", FSRoot: t.TempDir(), ArtifactPrefix: "artifacts"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "different-name.ext4")
	if err := os.WriteFile(file, []byte("rootfs"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := store.Upload(ctx, "artifacts/custom/rootfs.data", file, "")
	if err != nil {
		t.Fatal(err)
	}
	legacyKey, err := store.Upload(ctx, "artifacts/rfs-legacy.ext4", file, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		client     *s3store.Client
		artifactID string
		storedKey  string
		enabled    bool
		wantKey    string
	}{
		{"stored", store, "rfs-test", key, true, key},
		{"legacy-no-key", store, "rfs-legacy", "", true, legacyKey},
		{"local-only", nil, "rfs-test", key, true, ""},
		{"disabled", store, "rfs-test", key, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				received <- payload
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			reporter := newTestReporter(t, server)
			artifact := &models.RootfsArtifact{ArtifactID: tc.artifactID, RootfsFormat: "ext4", RootfsPath: file, RootfsSizeBytes: 6, ObjectKey: tc.storedKey}
			if err := reportExistingArtifact(ctx, "job-1", artifact, "fp", &image.PreparedSource{}, reporter, nil, "", tc.enabled, tc.client, log.G(ctx)); err != nil {
				t.Fatal(err)
			}
			payload := <-received
			gotKey, _ := payload["object_key"].(string)
			if gotKey != tc.wantKey || payload["rootfs_path"] != file || payload["ext4_path"] != file {
				t.Fatalf("callback metadata=%v", payload)
			}
		})
	}
}

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
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

// Regression coverage for the P1 bug where READY-artifact reuse only ever
// checked os.Stat(Ext4Path), so an S3-backed artifact built by a sibling TC
// replica (no shared local disk) was always treated as "missing" and
// silently rebuilt instead of reused.

func TestArtifactDataExistsS3ObjectConfirmed(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:  "rfs-1",
		ArtifactURL: "https://s3.example.com/bucket/rfs-1",
		Ext4Path:    filepath.Join(t.TempDir(), "does-not-exist.ext4"), // local copy absent on this replica
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
		Ext4Path:    filepath.Join(t.TempDir(), "does-not-exist.ext4"),
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
		Ext4Path:    ext4Path,
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
		Ext4Path:       filepath.Join(t.TempDir(), "does-not-exist.ext4"),
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
		Ext4Path:   ext4Path,
		// ArtifactURL empty: local-only artifact, no S3 involved.
	}

	if _, ok := artifactDataExists(artifact, nil); !ok {
		t.Fatal("expected reuse for a local-only artifact whose ext4 file exists")
	}

	artifact.Ext4Path = filepath.Join(dir, "missing.ext4")
	if _, ok := artifactDataExists(artifact, nil); ok {
		t.Fatal("expected no-reuse for a local-only artifact whose ext4 file is missing")
	}
}

type fakeArtifactS3 struct {
	objects map[string]bool
}

func (f *fakeArtifactS3) Stat(_ context.Context, artifactID string) (bool, error) {
	return f.objects[artifactID], nil
}

func (f *fakeArtifactS3) BackendName() string { return "s3" }
func (f *fakeArtifactS3) Bucket() string      { return "test-artifacts" }
func (f *fakeArtifactS3) FullObjectKey(artifactID string) string {
	return "template-artifacts/" + artifactID + ".ext4"
}

func TestReportArtifactBuiltStoresStableMetadata(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	reporter := &Reporter{masterURL: server.URL, httpClient: server.Client()}
	store := &fakeArtifactS3{objects: map[string]bool{"rfs-1": true}}
	for _, inStore := range []bool{true, false} {
		if err := reportArtifactBuilt(context.Background(), "job-1", &image.BuildResult{
			Ext4Path: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4", SHA256: "checksum", SizeBytes: 10,
		}, "rfs-1", "fingerprint", &image.PreparedSource{Digest: "digest", MasterNodeIP: "http://master"},
			reporter, cube_egress_ca.Result{}, inStore, store, "", nil, log.G(context.Background())); err != nil {
			t.Fatal(err)
		}
		if inStore {
			want := blobstore.ObjectLocator("s3", store.FullObjectKey("rfs-1"))
			if payload["artifact_url"] != want || payload["storage_backend"] != "s3" || payload["object_key"] != store.FullObjectKey("rfs-1") || payload["bucket"] != store.Bucket() {
				t.Fatalf("stored object must report stable locator/backend/key, got %+v", payload)
			}
		} else if payload["artifact_url"] != nil || payload["storage_backend"] != nil || payload["object_key"] != nil || payload["bucket"] != nil {
			t.Fatalf("failed upload must remain local, got %+v", payload)
		}
	}
}

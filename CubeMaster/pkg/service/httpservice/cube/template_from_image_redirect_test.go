// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

// stubRedirectLookup swaps the artifact lookup seam for the duration of a test.
func stubRedirectLookup(t *testing.T, fn func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error)) {
	t.Helper()
	old := getRootfsArtifactForRedirectFn
	getRootfsArtifactForRedirectFn = fn
	t.Cleanup(func() { getRootfsArtifactForRedirectFn = old })
}

func newRedirectContext(query string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/cube/template/artifact/download?"+query, nil)
	c.Request = req
	return c, w
}

func TestRedirectToS3ArtifactMissingArtifactID(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		t.Fatalf("lookup must not be called when artifact_id is empty")
		return nil, nil
	})
	c, w := newRedirectContext("token=abc")
	if redirectToS3Artifact(c) {
		t.Fatalf("expected false when artifact_id missing")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written, got code=%d", w.Code)
	}
}

func TestRedirectToS3ArtifactLookupError(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, gorm.ErrRecordNotFound
	})
	c, w := newRedirectContext("artifact_id=rfs-x&token=t")
	if redirectToS3Artifact(c) {
		t.Fatalf("expected false on lookup error")
	}
	// Must not write a partial response (the local-file path writes its own).
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on lookup error, got code=%d", w.Code)
	}
}

func TestRedirectToS3ArtifactInvalidToken(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, errors.New("invalid artifact token")
	})
	c, w := newRedirectContext("artifact_id=rfs-x&token=wrong")
	if redirectToS3Artifact(c) {
		t.Fatalf("expected false on invalid token")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on invalid token, got code=%d", w.Code)
	}
}

func TestRedirectToS3ArtifactNoArtifactURL(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		// Legacy/local-disk artifact: no S3 URL -> fall through to local stream.
		return &models.RootfsArtifact{ArtifactID: "rfs-x", ArtifactURL: ""}, nil
	})
	c, w := newRedirectContext("artifact_id=rfs-x&token=t")
	if redirectToS3Artifact(c) {
		t.Fatalf("expected false when artifact_url empty (local-disk artifact)")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written for local artifact, got code=%d", w.Code)
	}
}

func TestRedirectToS3ArtifactSuccess(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:  "rfs-abc",
			ArtifactURL: "https://minio:9000/bucket/rfs-abc.ext4?X-Amz-Signature=xyz",
			Ext4SHA256:  "deadbeef",
		}, nil
	})
	c, w := newRedirectContext("artifact_id=rfs-abc&token=t")
	if !redirectToS3Artifact(c) {
		t.Fatalf("expected true on successful redirect")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://minio:9000/bucket/rfs-abc.ext4?X-Amz-Signature=xyz" {
		t.Fatalf("Location=%q", loc)
	}
	if got := w.Header().Get("X-Cube-Artifact-Id"); got != "rfs-abc" {
		t.Fatalf("X-Cube-Artifact-Id=%q", got)
	}
	if got := w.Header().Get("ETag"); got != "deadbeef" {
		t.Fatalf("ETag=%q", got)
	}
}

func TestRedirectToS3ArtifactNilRecord(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, nil // nil record, nil error
	})
	c, w := newRedirectContext("artifact_id=rfs-x&token=t")
	if redirectToS3Artifact(c) {
		t.Fatalf("expected false on nil record")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on nil record, got code=%d", w.Code)
	}
}

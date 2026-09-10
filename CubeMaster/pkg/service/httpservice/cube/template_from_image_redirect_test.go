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

func newArtifactProxyContext(method, query string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(method, "/cube/template/artifact/download?"+query, nil)
	c.Request = req
	return c, w
}

func TestProxyS3ArtifactMissingArtifactID(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		t.Fatalf("lookup must not be called when artifact_id is empty")
		return nil, nil
	})
	c, w := newArtifactProxyContext(http.MethodGet, "token=abc")
	if handled, _ := proxyS3Artifact(c); handled {
		t.Fatalf("expected handled=false when artifact_id missing")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written, got code=%d", w.Code)
	}
}

func TestProxyS3ArtifactLookupError(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, gorm.ErrRecordNotFound
	})
	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-x&token=t")
	if handled, _ := proxyS3Artifact(c); handled {
		t.Fatalf("expected handled=false on lookup error")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on lookup error, got code=%d", w.Code)
	}
}

func TestProxyS3ArtifactInvalidToken(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, errors.New("invalid artifact token")
	})
	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-x&token=wrong")
	if handled, _ := proxyS3Artifact(c); handled {
		t.Fatalf("expected handled=false on invalid token")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on invalid token, got code=%d", w.Code)
	}
}

func TestProxyS3ArtifactNoArtifactURL(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{ArtifactID: "rfs-x", ArtifactURL: ""}, nil
	})
	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-x&token=t")
	if handled, _ := proxyS3Artifact(c); handled {
		t.Fatalf("expected handled=false when artifact_url empty (local-disk artifact)")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written for local artifact, got code=%d", w.Code)
	}
}

func TestProxyS3ArtifactSuccessGET(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Fatalf("Range=%q", got)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer upstream.Close()

	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:  "rfs-abc",
			ArtifactURL: upstream.URL + "/bucket/rfs-abc.ext4?X-Amz-Signature=xyz",
			Ext4SHA256:  "deadbeef",
		}, nil
	})

	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-abc&token=t")
	c.Request.Header.Set("Range", "bytes=0-0")
	handled, ok := proxyS3Artifact(c)
	if !handled || !ok {
		t.Fatalf("expected handled=true ok=true on successful proxy, got %v/%v", handled, ok)
	}
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", w.Code)
	}
	if body := w.Body.String(); body != "x" {
		t.Fatalf("body=%q", body)
	}
	if got := w.Header().Get("X-Cube-Artifact-Id"); got != "rfs-abc" {
		t.Fatalf("X-Cube-Artifact-Id=%q", got)
	}
	if got := w.Header().Get("ETag"); got != "deadbeef" {
		t.Fatalf("ETag=%q", got)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-0/1" {
		t.Fatalf("Content-Range=%q", got)
	}
}

func TestProxyS3ArtifactSuccessHEAD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The presigned URL is signed for GET (SigV4 binds the method), so a
		// HEAD probe must be proxied as a GET whose body is discarded.
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s, want GET (HEAD is proxied as GET)", r.Method)
		}
		if got := r.Header.Get("If-None-Match"); got != "client-etag" {
			t.Fatalf("If-None-Match=%q", got)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "123")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:  "rfs-head",
			ArtifactURL: upstream.URL + "/bucket/rfs-head.ext4?X-Amz-Signature=xyz",
			Ext4SHA256:  "sha-head",
		}, nil
	})

	c, w := newArtifactProxyContext(http.MethodHead, "artifact_id=rfs-head&token=t")
	c.Request.Header.Set("If-None-Match", "client-etag")
	handled, ok := proxyS3Artifact(c)
	if !handled || !ok {
		t.Fatalf("expected handled=true ok=true on successful head proxy, got %v/%v", handled, ok)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("HEAD body=%q", body)
	}
	if got := w.Header().Get("Content-Length"); got != "123" {
		t.Fatalf("Content-Length=%q", got)
	}
	if got := w.Header().Get("ETag"); got != "sha-head" {
		t.Fatalf("ETag=%q", got)
	}
}

func TestProxyS3ArtifactNilRecord(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return nil, nil
	})
	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-x&token=t")
	if handled, _ := proxyS3Artifact(c); handled {
		t.Fatalf("expected handled=false on nil record")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written on nil record, got code=%d", w.Code)
	}
}

// An upstream failure writes 502 to the client and must be reported as
// ok=false so the request log does not record a broken download as success.
func TestProxyS3ArtifactUpstreamFailureReportedAsFailure(t *testing.T) {
	stubRedirectLookup(t, func(ctx context.Context, artifactID, token string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:  "rfs-down",
			ArtifactURL: "http://127.0.0.1:1/bucket/rfs-down.ext4?X-Amz-Signature=xyz",
			Ext4SHA256:  "sha-down",
		}, nil
	})

	c, w := newArtifactProxyContext(http.MethodGet, "artifact_id=rfs-down&token=t")
	handled, ok := proxyS3Artifact(c)
	if !handled {
		t.Fatalf("expected handled=true on upstream failure (502 was written)")
	}
	if ok {
		t.Fatalf("expected ok=false on upstream failure")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}

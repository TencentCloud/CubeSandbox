// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package httpservice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/api"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
)

func TestArtifactDownloadHandlerOnlyUsesComponentAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte("rootfs-bytes")
	path := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:    "artifact-1",
		Generation:    2,
		Ext4Path:      path,
		Ext4SizeBytes: int64(len(body)),
		Ext4SHA256:    "abc123",
		Status:        templatecenter.ArtifactStatusReady,
	}
	oldLoader := loadDirectDownloadArtifact
	loadDirectDownloadArtifact = func(_ *gin.Context, artifactID string) (*models.RootfsArtifact, error) {
		if artifactID != artifact.ArtifactID {
			t.Fatalf("artifact id = %q", artifactID)
		}
		copy := *artifact
		return &copy, nil
	}
	t.Cleanup(func() { loadDirectDownloadArtifact = oldLoader })
	secret := strings.Repeat("tc-control-key-", 3)
	t.Setenv(constants.TemplateCallbackTokenEnv, secret)
	router := gin.New()
	api.InternalAPIGroup(router.Group("")).GET("/artifacts/:artifact_id/ext4", artifactDownloadHandler)
	request := func(componentToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/tc/api/v1/artifacts/artifact-1/ext4", nil)
		req.Header.Set(constants.TemplateCallbackTokenHeader, componentToken)
		req.Header.Set("X-Cube-Artifact-Token", "not-a-ticket")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	ok := request(secret)
	if ok.Code != http.StatusOK || ok.Body.String() != string(body) {
		t.Fatalf("component download = status %d body %q", ok.Code, ok.Body.String())
	}
	if got := request("").Code; got != http.StatusUnauthorized {
		t.Fatalf("node ticket alone status = %d, want 401", got)
	}
	if got := request("wrong-component-secret").Code; got != http.StatusUnauthorized {
		t.Fatalf("wrong component credential status = %d, want 401", got)
	}
	if got := ok.Header().Get("ETag"); got != `"abc123"` {
		t.Fatalf("ETag = %q", got)
	}
	for _, tc := range []struct {
		match  string
		status int
		body   string
	}{
		{`"abc123"`, http.StatusPartialContent, string(body[2:])},
		{`"old-generation"`, http.StatusOK, string(body)},
	} {
		req := httptest.NewRequest(http.MethodGet, "/tc/api/v1/artifacts/artifact-1/ext4", nil)
		req.Header.Set(constants.TemplateCallbackTokenHeader, secret)
		req.Header.Set("Range", "bytes=2-")
		req.Header.Set("If-Range", tc.match)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.status || rec.Body.String() != tc.body {
			t.Fatalf("If-Range %s: %d %q", tc.match, rec.Code, rec.Body.String())
		}
	}
	artifact.Status = templatecenter.ArtifactStatusFailed
	if got := request(secret).Code; got != http.StatusGone {
		t.Fatalf("non-ready artifact status = %d, want 410", got)
	}
}

// TestNodeArtifactDownloadHandlerUsesPerNodeTokenNotComponentSecret proves
// the direct-mode node route (/internal/artifacts/:id/ext4) authenticates
// with the per-node, per-generation download token verified against the
// shared store/node-cache (VerifyArtifactDownloadToken) — never the
// TC-wide component/callback secret that TestArtifactDownloadHandlerOnlyUsesComponentAuthentication
// guards on the other route.
func TestNodeArtifactDownloadHandlerUsesPerNodeTokenNotComponentSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte("rootfs-bytes")
	path := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:    "artifact-1",
		Generation:    2,
		Ext4Path:      path,
		Ext4SizeBytes: int64(len(body)),
		Ext4SHA256:    "abc123",
		Status:        templatecenter.ArtifactStatusReady,
	}
	oldVerify := verifyNodeArtifactDownload
	t.Cleanup(func() { verifyNodeArtifactDownload = oldVerify })
	verifyNodeArtifactDownload = func(_ context.Context, artifactID, nodeID, token string) (*models.RootfsArtifact, error) {
		if artifactID != artifact.ArtifactID {
			t.Fatalf("artifact id = %q", artifactID)
		}
		if nodeID != "node-1" || token != "valid-download-token" {
			return nil, templatecenter.ErrArtifactDownloadUnauthorized
		}
		copy := *artifact
		return &copy, nil
	}

	router := gin.New()
	router.GET("/internal/artifacts/:artifact_id/ext4", nodeArtifactDownloadHandler)
	request := func(nodeID, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/internal/artifacts/artifact-1/ext4", nil)
		if nodeID != "" {
			req.Header.Set(artifactticket.DownloadNodeIDHeader, nodeID)
		}
		if token != "" {
			req.Header.Set(artifactticket.DownloadTokenHeader, token)
		}
		// A component/callback secret alone must never substitute for the
		// per-node token on this route.
		req.Header.Set(constants.TemplateCallbackTokenHeader, "component-secret-should-be-ignored")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	ok := request("node-1", "valid-download-token")
	if ok.Code != http.StatusOK || ok.Body.String() != string(body) {
		t.Fatalf("valid node token = status %d body %q", ok.Code, ok.Body.String())
	}
	if got := request("node-1", "wrong-token").Code; got != http.StatusForbidden {
		t.Fatalf("forged token status = %d, want 403", got)
	}
	if got := request("other-node", "valid-download-token").Code; got != http.StatusForbidden {
		t.Fatalf("wrong node status = %d, want 403", got)
	}
	if got := request("", "valid-download-token").Code; got != http.StatusBadRequest {
		t.Fatalf("missing node id status = %d, want 400", got)
	}
	if got := request("node-1", "").Code; got != http.StatusBadRequest {
		t.Fatalf("missing token status = %d, want 400", got)
	}

	verifyNodeArtifactDownload = func(context.Context, string, string, string) (*models.RootfsArtifact, error) {
		return nil, errors.New("boom")
	}
	if got := request("node-1", "valid-download-token").Code; got != http.StatusInternalServerError {
		t.Fatalf("unexpected verify error status = %d, want 500", got)
	}
}

func TestDirectArtifactPathConstrainsFSObjectKey(t *testing.T) {
	root := t.TempDir()
	t.Setenv(configenv.EnvArtifactStoreFSRoot, root)
	valid := &models.RootfsArtifact{
		ArtifactID:     "artifact-1",
		StorageBackend: "fs",
		ObjectKey:      "templates/artifact-1.ext4",
	}
	got, err := directArtifactPath(valid)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "_data", "templates", "artifact-1.ext4")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	stableRoot := t.TempDir()
	valid.StorageRef = "file://" + stableRoot
	got, err = directArtifactPath(valid)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stableRoot, "_data", "templates", "artifact-1.ext4"); got != want {
		t.Fatalf("stable-locator path = %q, want %q", got, want)
	}

	valid.ObjectKey = "../escape.ext4"
	if _, err := directArtifactPath(valid); err == nil {
		t.Fatal("escaping object key unexpectedly accepted")
	}
	valid.StorageBackend = "s3"
	valid.ObjectKey = "artifact.ext4"
	if _, err := directArtifactPath(valid); err == nil {
		t.Fatal("s3 artifact unexpectedly mapped to a local path")
	}
}

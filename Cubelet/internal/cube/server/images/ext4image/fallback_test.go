// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// directFallbackSpec builds a spec that carries both a direct source and the
// stable CubeMaster url, which is what distribution emits for an upgraded
// node.
func directFallbackSpec(directURL, legacyURL, source string, payload []byte, expiresAt time.Time) *cubeimages.ImageSpec {
	sum := sha256.Sum256(payload)
	return &cubeimages.ImageSpec{Annotations: map[string]string{
		constants.MasterAnnotationRootfsArtifactURL:         legacyURL,
		constants.MasterAnnotationRootfsArtifactToken:       "legacy-token",
		constants.MasterAnnotationRootfsArtifactDirectURL:   directURL,
		constants.MasterAnnotationRootfsArtifactDirectToken: "short-capability",
		constants.MasterAnnotationRootfsArtifactSource:      source,
		constants.MasterAnnotationRootfsArtifactExpiresAt:   strconv.FormatInt(expiresAt.Unix(), 10),
		constants.MasterAnnotationRootfsArtifactSHA256:      hex.EncodeToString(sum[:]),
		constants.MasterAnnotationRootfsArtifactSizeBytes:   strconv.Itoa(len(payload)),
	}}
}

// A node that cannot reach the direct endpoint must still get the artifact
// from CubeMaster: without this, upgrading Cubelet turns "no node->S3 egress"
// into "template creation fails", with a rollback as the only remedy.
func TestDirectSourceUnreachableFallsBackToLegacyURL(t *testing.T) {
	payload := bytes.Repeat([]byte("artifact-bytes"), 128)
	var legacyHits int
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyHits++
		if got := r.Header.Get("X-Cube-Artifact-Token"); got != "legacy-token" {
			t.Errorf("legacy request token = %q, want the legacy token", got)
		}
		_, _ = w.Write(payload)
	}))
	defer legacy.Close()

	// Port 1 on loopback refuses connections, standing in for a node without
	// egress to the direct source.
	spec := directFallbackSpec("http://127.0.0.1:1/direct", legacy.URL, "tc", payload, time.Now().Add(5*time.Minute))
	imagePath := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := tryDownloadPmemFile(context.Background(), imagePath, spec); err != nil {
		t.Fatalf("expected the legacy fallback to succeed: %v", err)
	}
	if legacyHits != 1 {
		t.Fatalf("legacy source was requested %d time(s), want exactly 1", legacyHits)
	}
	got, err := os.ReadFile(imagePath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("artifact content mismatch after fallback: err=%v", err)
	}
}

// A rejected or missing capability, and a CubeMaster that predates the direct
// route, must both fall back rather than fail the build.
func TestDirectSourceErrorStatusesFallBackToLegacyURL(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusTooManyRequests, http.StatusBadGateway,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			payload := bytes.Repeat([]byte("fallback"), 64)
			direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer direct.Close()
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			defer legacy.Close()

			spec := directFallbackSpec(direct.URL, legacy.URL, "tc", payload, time.Now().Add(5*time.Minute))
			imagePath := filepath.Join(t.TempDir(), "artifact.ext4")
			if err := tryDownloadPmemFile(context.Background(), imagePath, spec); err != nil {
				t.Fatalf("status %d must fall back to the legacy url: %v", status, err)
			}
			got, err := os.ReadFile(imagePath)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("artifact content mismatch after %d fallback: err=%v", status, err)
			}
		})
	}
}

// An expired capability is reported as expiry and skipped before any request,
// instead of surfacing as an opaque 403 or a size mismatch.
func TestExpiredCapabilitySkipsDirectSourceBeforeRequesting(t *testing.T) {
	payload := bytes.Repeat([]byte("expired-capability"), 32)
	directHits := 0
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer direct.Close()
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer legacy.Close()

	spec := directFallbackSpec(direct.URL, legacy.URL, "tc", payload, time.Now().Add(-time.Second))
	imagePath := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := tryDownloadPmemFile(context.Background(), imagePath, spec); err != nil {
		t.Fatalf("expected the legacy url to serve the artifact: %v", err)
	}
	if directHits != 0 {
		t.Fatalf("an expired capability must not be sent, got %d request(s)", directHits)
	}
}

func TestExpiredCapabilityWithoutFallbackReportsExpiry(t *testing.T) {
	payload := []byte("no-fallback")
	spec := directFallbackSpec("http://127.0.0.1:1/direct", "", "tc", payload, time.Now().Add(-time.Minute))
	err := tryDownloadPmemFile(context.Background(), filepath.Join(t.TempDir(), "artifact.ext4"), spec)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected a clear expiry error, got %v", err)
	}
}

// Integrity failures must not be retried elsewhere: the bytes were served but
// do not match the artifact, so a fallback would only mask the corruption.
func TestCorruptDirectSourceDoesNotFallBack(t *testing.T) {
	payload := bytes.Repeat([]byte("expected-bytes"), 64)
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("corrupted-data"), 64))
	}))
	defer direct.Close()
	legacyHits := 0
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyHits++
		_, _ = w.Write(payload)
	}))
	defer legacy.Close()

	spec := directFallbackSpec(direct.URL, legacy.URL, "tc", payload, time.Now().Add(5*time.Minute))
	err := tryDownloadPmemFile(context.Background(), filepath.Join(t.TempDir(), "artifact.ext4"), spec)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected a digest mismatch to be reported, got %v", err)
	}
	if legacyHits != 0 {
		t.Fatalf("a corrupt payload must not trigger a fallback, got %d legacy request(s)", legacyHits)
	}
}

// The fallback must not hand the legacy Cube token to an object store.
func TestFallbackToS3SourceDoesNotSendCubeToken(t *testing.T) {
	payload := bytes.Repeat([]byte("s3-bytes"), 64)
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Cube-Artifact-Token"); got != "" {
			t.Errorf("object store received a Cube token %q", got)
		}
		_, _ = w.Write(payload)
	}))
	defer s3.Close()

	spec := directFallbackSpec(s3.URL, "http://127.0.0.1:1/legacy", "s3", payload, time.Now().Add(5*time.Minute))
	imagePath := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := tryDownloadPmemFile(context.Background(), imagePath, spec); err != nil {
		t.Fatalf("direct S3 download failed: %v", err)
	}
}

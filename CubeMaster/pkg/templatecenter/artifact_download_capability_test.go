// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestArtifactDownloadCapabilityIsArtifactBoundAndExpires(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	now := time.Unix(1_800_000_000, 0)
	token, err := NewArtifactDownloadCapability("artifact-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidArtifactDownloadCapability("artifact-1", token, now.Add(ArtifactDownloadCapabilityTTL-time.Second)) {
		t.Fatal("fresh capability was rejected before expiry")
	}
	if ValidArtifactDownloadCapability("artifact-1", token, now.Add(ArtifactDownloadCapabilityTTL+time.Second)) {
		t.Fatal("expired capability was accepted")
	}
	if ValidArtifactDownloadCapability("artifact-2", token, now) {
		t.Fatal("capability was accepted for a different artifact")
	}
	if ValidArtifactDownloadCapability("artifact-1", token+"x", now) {
		t.Fatal("tampered capability was accepted")
	}
}

func TestArtifactDownloadCapabilityAcceptsConfiguredHourTTL(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	now := time.Unix(1_800_000_000, 0)
	token, err := NewArtifactDownloadCapabilityWithTTL("artifact-1", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidArtifactDownloadCapability("artifact-1", token, now.Add(time.Hour-time.Second)) {
		t.Fatal("one-hour capability was rejected before expiry")
	}
	if ValidArtifactDownloadCapability("artifact-1", token, now.Add(time.Hour+time.Second)) {
		t.Fatal("one-hour capability remained valid after expiry")
	}
}

func TestNewArtifactDownloadCapabilityRequiresSharedSecret(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	if _, err := NewArtifactDownloadCapability("artifact-1", time.Now()); err == nil {
		t.Fatal("expected missing shared-secret error")
	}
}

func TestIssueArtifactDownloadSourceUsesFreshS3Presign(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	t.Cleanup(func() { presignArtifactGetURLWithTTL = oldSigner })
	var gotTTL time.Duration
	presignArtifactGetURLWithTTL = func(_ context.Context, artifact *models.RootfsArtifact, ttl time.Duration) (string, error) {
		if artifact.ArtifactID != "artifact-s3" {
			t.Fatalf("signer artifact id = %q", artifact.ArtifactID)
		}
		gotTTL = ttl
		return "https://bucket.s3.example/artifact.ext4?X-Amz-Signature=short", nil
	}
	now := time.Unix(1_800_000_000, 0)
	source, err := IssueArtifactDownloadSource(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-s3", StorageBackend: "s3", ArtifactURL: "https://old.example/object?signature=expired",
	}, "https://master.example", now)
	if err != nil {
		t.Fatal(err)
	}
	if gotTTL != ArtifactDownloadCapabilityTTL {
		t.Fatalf("presign ttl = %s, want %s", gotTTL, ArtifactDownloadCapabilityTTL)
	}
	if source.Kind != ArtifactSourceS3 || source.URL != "https://bucket.s3.example/artifact.ext4?X-Amz-Signature=short" || source.Token != "" {
		t.Fatalf("unexpected source: %+v", source)
	}
	if source.ExpiresAt != now.Add(ArtifactDownloadCapabilityTTL).Unix() {
		t.Fatalf("expires_at = %d", source.ExpiresAt)
	}
}

func TestIssueArtifactDownloadSourceUsesConfiguredTTLForS3(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	t.Cleanup(func() { presignArtifactGetURLWithTTL = oldSigner })
	var gotTTL time.Duration
	presignArtifactGetURLWithTTL = func(_ context.Context, _ *models.RootfsArtifact, ttl time.Duration) (string, error) {
		gotTTL = ttl
		return "https://bucket.s3.example/artifact.ext4?X-Amz-Signature=short", nil
	}
	now := time.Unix(1_800_000_000, 0)
	configuredTTL := 45 * time.Minute
	source, err := IssueArtifactDownloadSourceWithTTL(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-s3", StorageBackend: "s3", ArtifactURL: "s3://bucket/artifact.ext4",
	}, "https://master.example", configuredTTL, now)
	if err != nil {
		t.Fatal(err)
	}
	if gotTTL != configuredTTL || source.ExpiresAt != now.Add(configuredTTL).Unix() {
		t.Fatalf("signer TTL=%s source expires=%d, want TTL=%s", gotTTL, source.ExpiresAt, configuredTTL)
	}
}

func TestIssueArtifactDownloadSourceUsesTCWithShortCapability(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	oldTCURL := artifactTCSourceBaseURL
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactTCSourceBaseURL = oldTCURL
	})
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", nil
	}
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }
	now := time.Unix(1_800_000_000, 0)
	source, err := issueArtifactDownloadSourceWithOptions(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-fs", StorageBackend: "fs", DownloadToken: "legacy-token",
	}, "https://master.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceTC || source.URL != "https://tc.example.internal/cube/template/artifact/direct-download?artifact_id=artifact-fs" {
		t.Fatalf("unexpected TC source: %+v", source)
	}
	if !ValidArtifactDownloadCapability("artifact-fs", source.Token, now) {
		t.Fatal("TC source token is not a valid short-lived capability")
	}
	if strings.Contains(source.URL, source.Token) || source.Token == "legacy-token" {
		t.Fatal("TC capability leaked into the URL or used the persistent token")
	}
}

func TestIssueArtifactDownloadSourceUsesTCWhenConfigured(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	oldTCURL := artifactTCSourceBaseURL
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactTCSourceBaseURL = oldTCURL
	})
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", nil
	}
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }
	now := time.Unix(1_800_000_000, 0)
	ttl := 45 * time.Minute
	source, err := issueArtifactDownloadSourceWithOptions(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-fs", StorageBackend: "fs", DownloadToken: "legacy-token",
	}, "https://master.example", ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceTC || source.ExpiresAt != now.Add(ttl).Unix() {
		t.Fatalf("TC source/expiry mismatch: %+v", source)
	}
	if !ValidArtifactDownloadCapability("artifact-fs", source.Token, now) {
		t.Fatal("TC source token is not valid for configured TTL")
	}
}

func TestIssueArtifactDownloadSourceFallsBackToMasterWhenTCAddressMissing(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldTCURL := artifactTCSourceBaseURL
	t.Cleanup(func() { artifactTCSourceBaseURL = oldTCURL })
	artifactTCSourceBaseURL = func() string { return "" }
	source, err := IssueArtifactDownloadSource(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-fs", StorageBackend: "fs", DownloadToken: "legacy-token",
	}, "https://master.example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceMaster || !strings.HasPrefix(source.URL, "https://master.example/cube/template/artifact/direct-download?") {
		t.Fatalf("missing TC endpoint should select Master, got %+v", source)
	}
}

func TestSetArtifactDownloadAnnotationsKeepsLegacyAndDirectSourcesSeparate(t *testing.T) {
	legacyURL := "https://master.example/cube/template/artifact/download?artifact_id=a&token=legacy"
	direct := &ArtifactDownloadSource{
		URL: "https://s3.example/artifact?signature=short", Token: "capability",
		Kind: ArtifactSourceS3, ExpiresAt: 1_800_003_600,
	}
	annotations := make(map[string]string)
	setArtifactDownloadAnnotations(annotations, legacyURL, "legacy", direct)
	if annotations[constants.CubeAnnotationRootfsArtifactURL] != legacyURL || annotations[constants.CubeAnnotationRootfsArtifactToken] != "legacy" {
		t.Fatalf("legacy-compatible annotations were replaced: %+v", annotations)
	}
	if annotations[constants.CubeAnnotationRootfsArtifactDirectURL] != direct.URL ||
		annotations[constants.CubeAnnotationRootfsArtifactDirectToken] != direct.Token ||
		annotations[constants.CubeAnnotationRootfsArtifactSource] != ArtifactSourceS3 {
		t.Fatalf("direct source annotations are missing: %+v", annotations)
	}

	setArtifactDownloadAnnotations(annotations, legacyURL, "legacy", &ArtifactDownloadSource{Kind: ArtifactSourceLegacy})
	if annotations[constants.CubeAnnotationRootfsArtifactURL] != legacyURL || annotations[constants.CubeAnnotationRootfsArtifactDirectURL] != "" {
		t.Fatalf("legacy fallback annotations are invalid: %+v", annotations)
	}
	if _, ok := annotations[constants.CubeAnnotationRootfsArtifactSource]; ok {
		t.Fatalf("legacy fallback should omit direct source metadata: %+v", annotations)
	}
}

func TestIssueArtifactDownloadSourceUsesLegacyMasterFallbackWithoutSecret(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "")
	oldSigner := presignArtifactGetURLWithTTL
	oldTCURL := artifactTCSourceBaseURL
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactTCSourceBaseURL = oldTCURL
	})
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", nil
	}
	artifactTCSourceBaseURL = func() string { return "" }
	source, err := IssueArtifactDownloadSource(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-local", DownloadToken: "legacy-token",
	}, "https://master.example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceLegacy || !strings.Contains(source.URL, "token=legacy-token") || source.Token != "legacy-token" {
		t.Fatalf("unexpected legacy fallback: %+v", source)
	}
}

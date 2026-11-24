// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// TestArtifactTCSourceBaseURLRejectsLoopbackWithoutSharedNodeIP ensures a
// local one-click default can never be handed to remote Cubelets as a TC URL.
func TestArtifactTCSourceBaseURLRejectsLoopbackWithoutSharedNodeIP(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterNodeAddr, "http://127.0.0.1:8090")
	t.Setenv(sharedEnvNodeIP, "")
	if got := artifactTCSourceBaseURL(); got != "" {
		t.Fatalf("artifactTCSourceBaseURL() = %q, want empty for unreachable loopback", got)
	}
}

// A loopback TC address must NOT be rewritten to the shared node IP, even
// when one is available. one-click binds TC to 127.0.0.1 (only the co-located
// Master calls it) while CubeMaster binds 0.0.0.0, so nodeIP:8090 has no
// listener; handing it to Cubelets would break every template creation on a
// local-disk deployment. Falling back to the Master route keeps it working.
func TestArtifactTCSourceBaseURLDoesNotRewriteLoopbackToSharedNodeIP(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterNodeAddr, "http://127.0.0.1:8090")
	t.Setenv(sharedEnvNodeIP, "10.0.0.9")
	if got := artifactTCSourceBaseURL(); got != "" {
		t.Fatalf("artifactTCSourceBaseURL() = %q, want empty so selection falls back to the Master route", got)
	}
}

func TestArtifactTCSourceBaseURLRejectsWildcardBind(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterNodeAddr, "http://0.0.0.0:8090")
	t.Setenv(sharedEnvNodeIP, "10.0.0.9")
	if got := artifactTCSourceBaseURL(); got != "" {
		t.Fatalf("artifactTCSourceBaseURL() = %q, want empty for a wildcard bind", got)
	}
}

func TestArtifactTCSourceBaseURLAcceptsRoutableHost(t *testing.T) {
	t.Setenv(config.EnvTemplateCenterNodeAddr, "https://tc.example.internal:8090")
	if got, want := artifactTCSourceBaseURL(), "https://tc.example.internal:8090"; got != want {
		t.Fatalf("artifactTCSourceBaseURL() = %q, want %q", got, want)
	}
}

// TestIssueArtifactDownloadSourceS3PresignErrorFallsBackWithoutRemoteTier
// pins that a genuine presign failure (credentials revoked, bucket
// unreachable, etc.) on a standalone-Master deployment degrades to the
// Master HTTP path instead of failing the whole distribution.
//
// Earlier coverage only exercised the "no error, empty URL" case
// (blobstore.ErrUnsupported); a non-nil error from the signer was not
// covered, and the branch is reached on every production S3 outage.
func TestIssueArtifactDownloadSourceS3PresignErrorFallsBackWithoutRemoteTier(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	oldRemoteTier := artifactServedByRemoteTier
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactServedByRemoteTier = oldRemoteTier
	})
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", errors.New("s3: access denied")
	}
	artifactServedByRemoteTier = func() bool { return false }

	now := time.Unix(1_800_000_000, 0)
	source, err := IssueArtifactDownloadSource(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-s3-outage", StorageBackend: "s3", ArtifactURL: "s3://bucket/artifact.ext4",
	}, "https://master.example", now)
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceMaster {
		t.Fatalf("presign error without a remote TC tier should fall back to Master, got %+v", source)
	}
}

// TestIssueArtifactDownloadSourceS3PresignErrorFallsBackToTCOnRemoteTier
// mirrors the standalone-TC topology: once CubeMaster delegates artifact
// serving to the remote tier (CUBE_TEMPLATE_CENTER_ADDR configured), an S3
// presign failure must still route through TC rather than Master, because
// Master holds nothing on disk in that topology.
func TestIssueArtifactDownloadSourceS3PresignErrorFallsBackToTCOnRemoteTier(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	oldTCURL := artifactTCSourceBaseURL
	oldRemoteTier := artifactServedByRemoteTier
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactTCSourceBaseURL = oldTCURL
		artifactServedByRemoteTier = oldRemoteTier
	})
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", errors.New("s3: access denied")
	}
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }
	artifactServedByRemoteTier = func() bool { return true }

	now := time.Unix(1_800_000_000, 0)
	source, err := IssueArtifactDownloadSource(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-s3-outage-remote-tier", StorageBackend: "s3", ArtifactURL: "s3://bucket/artifact.ext4",
	}, "https://master.example", now)
	if err != nil {
		t.Fatal(err)
	}
	if source.Kind != ArtifactSourceTC {
		t.Fatalf("presign error on a remote TC tier should route to TC, got %+v", source)
	}
}

// TestIssueArtifactDownloadSourceConcurrentIsRaceFree exercises the exact
// fan-out pattern distributeRootfsArtifactToNodes uses in production: one
// goroutine per target node, all reading the same package-level
// source-selection seams (artifactTCSourceBaseURL, presignArtifactGetURLWithTTL)
// concurrently. Run with `go test -race` — this is the regression guard for
// that specific production shape, not a correctness check on its own.
func TestIssueArtifactDownloadSourceConcurrentIsRaceFree(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldTCURL := artifactTCSourceBaseURL
	t.Cleanup(func() { artifactTCSourceBaseURL = oldTCURL })
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }

	const nodeCount = 64
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-concurrent-fanout", StorageBackend: "fs"}
	now := time.Now()

	var wg sync.WaitGroup
	errs := make([]error, nodeCount)
	kinds := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			source, err := IssueArtifactDownloadSource(context.Background(), artifact, "https://master.example", now)
			errs[i] = err
			if source != nil {
				kinds[i] = source.Kind
			}
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		if kinds[i] != ArtifactSourceTC {
			t.Fatalf("node %d: kind = %q, want %q", i, kinds[i], ArtifactSourceTC)
		}
	}
}

// artifactDownloadScenario is one row of the production-readiness matrix: a
// deployment combination CubeMaster must handle correctly during a rolling
// CubeMaster/TC upgrade (S3 configured, TC-only, neither, or the TC shared
// secret not yet rolled out on this node).
type artifactDownloadScenario struct {
	name           string
	secret         string
	storageBackend string
	artifactURL    string
	s3PresignURL   string
	s3PresignErr   error
	tcBaseURL      string
	remoteTier     bool
	ttl            time.Duration
	wantKind       string
	wantHasToken   bool
}

// TestArtifactDownloadSourceProductionMatrix is the production-readiness
// scenario matrix for direct-passthrough (S3/TC) vs non-direct (Master or
// legacy) artifact distribution, run with `go test -v` to print the report
// table below. Every row also re-validates the annotation layer
// (setArtifactDownloadAnnotations): the legacy Master URL/token must always
// be present for un-upgraded Cubelets, and the new direct_* annotations must
// be present whenever a direct source was actually selected.
func TestArtifactDownloadSourceProductionMatrix(t *testing.T) {
	scenarios := []artifactDownloadScenario{
		{
			name:           "s3-direct-passthrough",
			secret:         "shared-template-secret",
			storageBackend: "s3",
			artifactURL:    "s3://bucket/artifact.ext4",
			s3PresignURL:   "https://bucket.s3.example/artifact.ext4?X-Amz-Signature=ok",
			ttl:            time.Hour,
			wantKind:       ArtifactSourceS3,
			wantHasToken:   false,
		},
		{
			name:           "fs-backend-direct-passthrough-via-tc",
			secret:         "shared-template-secret",
			storageBackend: "fs",
			tcBaseURL:      "https://tc.example.internal",
			ttl:            time.Hour,
			wantKind:       ArtifactSourceTC,
			wantHasToken:   true,
		},
		{
			name:           "standalone-tc-tier-routes-s3-outage-to-tc",
			secret:         "shared-template-secret",
			storageBackend: "s3",
			artifactURL:    "s3://bucket/artifact.ext4",
			s3PresignErr:   errors.New("s3: access denied"),
			tcBaseURL:      "https://tc.example.internal",
			remoteTier:     true,
			ttl:            time.Hour,
			wantKind:       ArtifactSourceTC,
			wantHasToken:   true,
		},
		{
			name:           "non-remote-tier-s3-outage-falls-back-to-master",
			secret:         "shared-template-secret",
			storageBackend: "s3",
			artifactURL:    "s3://bucket/artifact.ext4",
			s3PresignErr:   errors.New("s3: access denied"),
			tcBaseURL:      "https://tc.example.internal",
			remoteTier:     false,
			ttl:            time.Hour,
			wantKind:       ArtifactSourceMaster,
			wantHasToken:   true,
		},
		{
			name:           "tc-address-unconfigured-falls-back-to-master",
			secret:         "shared-template-secret",
			storageBackend: "fs",
			tcBaseURL:      "",
			ttl:            time.Hour,
			wantKind:       ArtifactSourceMaster,
			wantHasToken:   true,
		},
		{
			name:           "callback-secret-not-rolled-out-degrades-to-legacy",
			secret:         "",
			storageBackend: "fs",
			tcBaseURL:      "https://tc.example.internal",
			ttl:            time.Hour,
			wantKind:       ArtifactSourceLegacy,
			wantHasToken:   true,
		},
		{
			name:           "configured-short-ttl-is-honored-on-direct-tc",
			secret:         "shared-template-secret",
			storageBackend: "fs",
			tcBaseURL:      "https://tc.example.internal",
			ttl:            15 * time.Minute,
			wantKind:       ArtifactSourceTC,
			wantHasToken:   true,
		},
	}

	oldSigner := presignArtifactGetURLWithTTL
	oldTCURL := artifactTCSourceBaseURL
	oldRemoteTier := artifactServedByRemoteTier
	t.Cleanup(func() {
		presignArtifactGetURLWithTTL = oldSigner
		artifactTCSourceBaseURL = oldTCURL
		artifactServedByRemoteTier = oldRemoteTier
	})

	t.Logf("%-46s | %-11s | %-6s | %-5s | %-8s | %s",
		"scenario", "passthrough", "kind", "token", "ttl", "result")
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Setenv(constants.TemplateCallbackTokenEnv, sc.secret)
			presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
				if sc.s3PresignErr != nil {
					return "", sc.s3PresignErr
				}
				return sc.s3PresignURL, nil
			}
			artifactTCSourceBaseURL = func() string { return sc.tcBaseURL }
			artifactServedByRemoteTier = func() bool { return sc.remoteTier }

			artifact := &models.RootfsArtifact{
				ArtifactID:     "artifact-" + sc.name,
				StorageBackend: sc.storageBackend,
				ArtifactURL:    sc.artifactURL,
				DownloadToken:  "legacy-token-" + sc.name,
			}
			now := time.Unix(1_800_000_000, 0)
			source, err := IssueArtifactDownloadSourceWithTTL(
				context.Background(), artifact, "https://master.example", sc.ttl, now)

			passthrough, kind, hasToken, status := "no", "", false, "FAIL"
			if err == nil && source != nil {
				kind = source.Kind
				hasToken = source.Token != ""
				if kind == ArtifactSourceS3 || kind == ArtifactSourceTC {
					passthrough = "yes"
				}
				if kind == sc.wantKind && hasToken == sc.wantHasToken {
					status = "PASS"
				}
			}
			t.Logf("%-46s | %-11s | %-6s | %-5v | %-8s | %s",
				sc.name, passthrough, kind, hasToken, sc.ttl, status)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != sc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, sc.wantKind)
			}
			if hasToken != sc.wantHasToken {
				t.Fatalf("hasToken = %v, want %v", hasToken, sc.wantHasToken)
			}
			// The legacy fallback intentionally carries no expires_at: it
			// reuses the long-lived DB DownloadToken and never gets a
			// direct_expires_at annotation (see below), so there is nothing
			// for a Cubelet to validate against. Every other kind issues a
			// short-lived capability and must report its real expiry.
			if kind != ArtifactSourceLegacy && source.ExpiresAt != now.Add(sc.ttl).Unix() {
				t.Fatalf("expires_at = %d, want %d", source.ExpiresAt, now.Add(sc.ttl).Unix())
			}

			// Annotation-layer data validation: the legacy Master URL/token
			// must survive every scenario (old Cubelets never upgrade in
			// lockstep with CubeMaster), and the new direct_* annotations
			// must be present exactly when a direct source was selected.
			legacyURL := buildDownloadURL("https://master.example", artifact.ArtifactID, artifact.DownloadToken)
			annotations := make(map[string]string)
			setArtifactDownloadAnnotations(annotations, legacyURL, artifact.DownloadToken, source)
			if annotations[constants.CubeAnnotationRootfsArtifactURL] != legacyURL {
				t.Fatalf("legacy URL annotation missing (breaks un-upgraded Cubelets): %+v", annotations)
			}
			if annotations[constants.CubeAnnotationRootfsArtifactToken] != artifact.DownloadToken {
				t.Fatalf("legacy token annotation missing (breaks un-upgraded Cubelets): %+v", annotations)
			}
			wantDirectAnnotations := kind != ArtifactSourceLegacy
			gotDirectAnnotations := annotations[constants.CubeAnnotationRootfsArtifactDirectURL] != ""
			if gotDirectAnnotations != wantDirectAnnotations {
				t.Fatalf("direct URL annotation presence = %v, want %v (kind=%s): %+v",
					gotDirectAnnotations, wantDirectAnnotations, kind, annotations)
			}
		})
	}
}

// BenchmarkIssueArtifactDownloadSourceS3 measures the direct-passthrough S3
// path: one presign call plus struct construction, no HMAC signing.
func BenchmarkIssueArtifactDownloadSourceS3(b *testing.B) {
	b.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldSigner := presignArtifactGetURLWithTTL
	defer func() { presignArtifactGetURLWithTTL = oldSigner }()
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "https://bucket.s3.example/artifact.ext4?X-Amz-Signature=short", nil
	}
	artifact := &models.RootfsArtifact{
		ArtifactID: "artifact-bench-s3", StorageBackend: "s3", ArtifactURL: "s3://bucket/artifact.ext4",
	}
	now := time.Now()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := IssueArtifactDownloadSource(ctx, artifact, "https://master.example", now); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIssueArtifactDownloadSourceTC measures the direct-passthrough TC
// path: HMAC-SHA256 token signing plus URL construction, no network I/O.
func BenchmarkIssueArtifactDownloadSourceTC(b *testing.B) {
	b.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldTCURL := artifactTCSourceBaseURL
	defer func() { artifactTCSourceBaseURL = oldTCURL }()
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-bench-tc", StorageBackend: "fs"}
	now := time.Now()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := IssueArtifactDownloadSource(ctx, artifact, "https://master.example", now); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIssueArtifactDownloadSourceLegacyFallback measures the
// non-direct legacy Master path used when the TC shared secret has not been
// rolled out to this CubeMaster instance yet.
func BenchmarkIssueArtifactDownloadSourceLegacyFallback(b *testing.B) {
	b.Setenv(constants.TemplateCallbackTokenEnv, "")
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-bench-legacy", DownloadToken: "legacy-token"}
	now := time.Now()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := IssueArtifactDownloadSource(ctx, artifact, "https://master.example", now); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIssueArtifactDownloadSourceTCParallel measures the TC path under
// the concurrency shape distributeRootfsArtifactToNodes actually uses: many
// goroutines issuing sources for distinct nodes at once.
func BenchmarkIssueArtifactDownloadSourceTCParallel(b *testing.B) {
	b.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	oldTCURL := artifactTCSourceBaseURL
	defer func() { artifactTCSourceBaseURL = oldTCURL }()
	artifactTCSourceBaseURL = func() string { return "https://tc.example.internal" }
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-bench-tc-parallel", StorageBackend: "fs"}
	now := time.Now()
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := IssueArtifactDownloadSource(ctx, artifact, "https://master.example", now); err != nil {
				b.Fatal(err)
			}
		}
	})
}

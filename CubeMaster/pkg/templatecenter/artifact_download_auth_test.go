// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// The direct-download route is mounted OUTSIDE the user API-key middleware, so
// it has no second factor. The long-lived DownloadToken never expires, is
// stored in plaintext on the artifact row and is handed to every Cubelet that
// ever pulled the artifact -- accepting it there would turn one leak into
// permanent access to the artifact's bytes. The legacy route keeps accepting
// it, because the caller is already authenticated by then.
func TestArtifactDownloadTokenAuthorizationByRoute(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	now := time.Now()
	capability, err := NewArtifactDownloadCapability("rfs-1", now)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := NewArtifactDownloadCapabilityWithTTL("rfs-1", time.Second, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := NewArtifactDownloadCapability("rfs-2", now)
	if err != nil {
		t.Fatal(err)
	}
	record := &models.RootfsArtifact{ArtifactID: "rfs-1", DownloadToken: "long-lived-token"}

	cases := []struct {
		name            string
		token           string
		wantLegacyRoute bool
		wantDirectRoute bool
	}{
		{"short-lived capability works on both routes", capability, true, true},
		{"long-lived token is refused without API-key auth", "long-lived-token", true, false},
		{"an expired capability is refused everywhere", expired, false, false},
		{"a capability for another artifact is refused everywhere", foreign, false, false},
		{"an empty token is refused everywhere", "", false, false},
		{"a forged token is refused everywhere", "v1.9999999999.AAAA", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validRootfsArtifactDownloadToken(context.Background(), record, tc.token); got != tc.wantLegacyRoute {
				t.Fatalf("legacy (API-key authenticated) route = %v, want %v", got, tc.wantLegacyRoute)
			}
			directCtx := WithCapabilityOnlyArtifactAuth(context.Background())
			if got := validRootfsArtifactDownloadToken(directCtx, record, tc.token); got != tc.wantDirectRoute {
				t.Fatalf("direct-download route = %v, want %v", got, tc.wantDirectRoute)
			}
		})
	}
}

// A row with no stored DownloadToken must not make the empty string a valid
// credential on either route.
func TestArtifactDownloadTokenRejectsEmptyStoredToken(t *testing.T) {
	t.Setenv(constants.TemplateCallbackTokenEnv, "shared-template-secret")
	record := &models.RootfsArtifact{ArtifactID: "rfs-1"}
	for _, ctx := range []context.Context{
		context.Background(),
		WithCapabilityOnlyArtifactAuth(context.Background()),
	} {
		if validRootfsArtifactDownloadToken(ctx, record, "") {
			t.Fatal("an empty stored token must not authorize an anonymous download")
		}
	}
}

// The marker must not leak across requests: it is set per-request by the route
// middleware, so an unmarked context has to keep the permissive legacy
// behavior (otherwise every authenticated download would break).
func TestCapabilityOnlyArtifactAuthMarker(t *testing.T) {
	if CapabilityOnlyArtifactAuth(context.Background()) {
		t.Fatal("an unmarked context must not be capability-only")
	}
	//nolint:staticcheck // SA1012: nil ctx is defensive, callers pass c.Request.Context()
	if CapabilityOnlyArtifactAuth(nil) {
		t.Fatal("a nil context must not be capability-only")
	}
	if !CapabilityOnlyArtifactAuth(WithCapabilityOnlyArtifactAuth(context.Background())) {
		t.Fatal("a marked context must be capability-only")
	}
	// Marking must survive the derived contexts handlers create.
	derived, cancel := context.WithCancel(WithCapabilityOnlyArtifactAuth(context.Background()))
	defer cancel()
	if !CapabilityOnlyArtifactAuth(derived) {
		t.Fatal("the marker must survive context derivation")
	}
}

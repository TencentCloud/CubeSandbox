// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
	"gorm.io/gorm"
)

func TestArtifactDownloadRejectsDeletedReplicaAndForgedToken(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.RootfsArtifact{}, &models.TemplateReplica{}); err != nil {
		t.Fatal(err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	const nodeID = "ticket-auth-regression-node"
	oldLookup := lookupArtifactDownloadNode
	lookupArtifactDownloadNode = func(id string) (*node.Node, bool) { return &node.Node{}, id == nodeID }
	t.Cleanup(func() { lookupArtifactDownloadNode = oldLookup })
	t.Setenv(constants.ArtifactDownloadSigningKeyEnv, strings.Repeat("private-control-token-", 3))
	artifact := &models.RootfsArtifact{ArtifactID: "auth-artifact", Status: ArtifactStatusReady, Generation: 1,
		DownloadToken: strings.Repeat("node-visible-download-token-", 2), MasterNodeIP: "http://master.example:8089",
		StorageBackend: "fs", StorageRef: "file:///data/artifact.ext4", OwnerTCDownloadBase: "http://tc.example:8090"}
	replica := &models.TemplateReplica{TemplateID: "template-auth", ArtifactID: artifact.ArtifactID, NodeID: nodeID, Phase: ReplicaPhaseDistributing}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(replica).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	refresh, err := BuildArtifactDownloadRefreshToken(ctx, artifact, nodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := IssueArtifactDownloadTicket(ctx, artifact.ArtifactID, nodeID, refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArtifactDownloadToken(ctx, artifact.ArtifactID, nodeID, ticket.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArtifactDownloadToken(ctx, artifact.ArtifactID, nodeID, refresh); !errors.Is(err, ErrArtifactDownloadUnauthorized) {
		t.Fatal("refresh accepted as download")
	}
	nodeKey, err := artifactticket.DeriveSigningKey([]byte(artifact.DownloadToken))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := artifactticket.Sign(nodeKey, artifactticket.Claims{ArtifactID: artifact.ArtifactID, NodeID: nodeID,
		Generation: 1, Purpose: artifactticket.PurposeRefresh, ExpiresAt: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := IssueArtifactDownloadTicket(ctx, artifact.ArtifactID, nodeID, forged); !errors.Is(err, ErrArtifactDownloadUnauthorized) {
		t.Fatal("forged refresh accepted")
	}
	if err := db.Delete(replica).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := IssueArtifactDownloadTicket(ctx, artifact.ArtifactID, nodeID, refresh); !errors.Is(err, ErrArtifactDownloadUnauthorized) {
		t.Fatalf("deleted replica refresh: %v", err)
	}
	if _, err := VerifyArtifactDownloadToken(ctx, artifact.ArtifactID, nodeID, ticket.Token); !errors.Is(err, ErrArtifactDownloadUnauthorized) {
		t.Fatalf("deleted replica download: %v", err)
	}
}

func TestArtifactDownloadAnnotationsModeSnapshot(t *testing.T) {
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-1", MasterNodeIP: "http://master.example:8089", DownloadToken: "existing-token", StorageBackend: "fs", OwnerTCDownloadBase: "http://tc.internal:8080"}
	annotations, err := artifactDownloadAnnotations(context.Background(), artifact, "")
	if err != nil {
		t.Fatal(err)
	}
	if annotations[constants.CubeAnnotationRootfsArtifactTicketURL] == "" || annotations[constants.CubeAnnotationRootfsArtifactURL] != "" || annotations[constants.CubeAnnotationRootfsArtifactToken] != "" {
		t.Fatal("ticket protocol mixed with legacy annotations")
	}
	// Cubelet fetches the ticket from Master but downloads fs/TC bytes
	// straight from the owner TC: the pinned origin must be TC's, not
	// Master's ticket-issuing origin, or every download would fail
	// Cubelet's origin check.
	if annotations[constants.CubeAnnotationRootfsArtifactDownloadOrigin] != "http://tc.internal:8080" {
		t.Fatalf("fs origin = %q, want the owner TC origin", annotations[constants.CubeAnnotationRootfsArtifactDownloadOrigin])
	}
}

func setArtifactDownloadTTL(t *testing.T, ttl time.Duration) {
	t.Helper()
	old := artifactDownloadTTL
	artifactDownloadTTL = func() (time.Duration, error) { return ttl, nil }
	t.Cleanup(func() { artifactDownloadTTL = old })
}

func TestArtifactDownloadUsesExistingCredentialOnly(t *testing.T) {
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-1", DownloadToken: strings.Repeat("node-visible-token-", 3)}
	t.Setenv(constants.TemplateCallbackTokenEnv, strings.Repeat("separate-component-token-", 3))
	t.Setenv(constants.ArtifactDownloadSigningKeyEnv, strings.Repeat("control-token-", 3))
	key, err := artifactDownloadSigningKey(artifact)
	if err != nil {
		t.Fatal(err)
	}
	nodeKey, err := artifactticket.DeriveSigningKey([]byte(artifact.DownloadToken))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := artifactticket.Sign(nodeKey, artifactticket.Claims{ArtifactID: artifact.ArtifactID, NodeID: "other-node",
		Generation: 1, Purpose: artifactticket.PurposeDownload, ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactticket.Verify(key, forged, time.Now()); err == nil {
		t.Fatal("node-visible credential can forge tickets")
	}
	callbackKey, err := artifactticket.DeriveSigningKey([]byte(strings.Repeat("separate-component-token-", 3)))
	if err != nil {
		t.Fatal(err)
	}
	callbackForged, err := artifactticket.Sign(callbackKey, artifactticket.Claims{ArtifactID: artifact.ArtifactID, NodeID: "other-node",
		Generation: 1, Purpose: artifactticket.PurposeDownload, ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactticket.Verify(key, callbackForged, time.Now()); err == nil {
		t.Fatal("component callback credential can forge tickets")
	}
	artifact.DownloadToken = ""
	if _, err := artifactDownloadSigningKey(artifact); err != nil {
		t.Fatal("ticket key must not depend on legacy token")
	}
	for _, token := range []string{"", "weak"} {
		t.Setenv(constants.ArtifactDownloadSigningKeyEnv, token)
		if _, err := artifactDownloadSigningKey(artifact); err == nil {
			t.Fatal("missing/weak service secret accepted")
		}
	}
}

func TestBuildArtifactDownloadTicketForTemplateCenter(t *testing.T) {
	t.Setenv(constants.ArtifactDownloadSigningKeyEnv, strings.Repeat("download-key-", 3))
	secret, err := artifactticket.DeriveSigningKey([]byte(strings.Repeat("download-key-", 3)))
	if err != nil {
		t.Fatal(err)
	}
	setArtifactDownloadTTL(t, 2*time.Hour)
	now := time.Unix(1_800_000_000, 0)
	artifact := &models.RootfsArtifact{
		ArtifactID:          "artifact-1",
		Generation:          4,
		StorageBackend:      "fs",
		OwnerTCDownloadBase: "http://tc.internal:8080/base?ignored=1",
		MasterNodeIP:        "http://master.example:8089",
		DownloadToken:       strings.Repeat("download-key-", 3),
		Ext4SizeBytes:       1234,
		Ext4SHA256:          "abc123",
	}

	ticket, err := buildArtifactDownloadTicket(context.Background(), artifact, "node-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Method != "GET" ||
		ticket.Headers[artifactticket.DownloadTokenHeader] != ticket.Token ||
		ticket.Headers[artifactticket.DownloadNodeIDHeader] != "node-1" ||
		ticket.Source != artifactticket.SourceTemplateCenter ||
		ticket.URL != "http://tc.internal:8080/base/internal/artifacts/artifact-1/ext4" ||
		ticket.ExpiresAt.Sub(now) != 2*time.Hour ||
		ticket.SizeBytes != artifact.Ext4SizeBytes ||
		ticket.SHA256 != artifact.Ext4SHA256 {
		t.Fatalf("ticket = %+v", ticket)
	}
	claims, err := artifactticket.Verify(secret, ticket.Token, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ArtifactID != artifact.ArtifactID || claims.NodeID != "node-1" ||
		claims.Generation != artifact.Generation || claims.ExpiresAt != ticket.ExpiresAt.Unix() ||
		claims.Purpose != artifactticket.PurposeDownload {
		t.Fatalf("claims = %+v, ticket = %+v", claims, ticket)
	}
}

func TestBuildArtifactDownloadRefreshToken(t *testing.T) {
	t.Setenv(constants.ArtifactDownloadSigningKeyEnv, strings.Repeat("refresh-key-", 3))
	secret, err := artifactticket.DeriveSigningKey([]byte(strings.Repeat("refresh-key-", 3)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	token, err := BuildArtifactDownloadRefreshToken(context.Background(), &models.RootfsArtifact{
		ArtifactID:    "artifact-1",
		Generation:    7,
		DownloadToken: strings.Repeat("refresh-key-", 3),
	}, "node-1", now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := artifactticket.Verify(secret, token, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Purpose != artifactticket.PurposeRefresh ||
		claims.ArtifactID != "artifact-1" || claims.NodeID != "node-1" ||
		claims.Generation != 7 || claims.ExpiresAt != now.Add(artifactRefreshTokenTTL).Unix() {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestBuildArtifactDownloadTicketForS3UsesFreshBoundedURL(t *testing.T) {
	setArtifactDownloadTTL(t, 2*time.Hour)
	oldPresign := presignArtifactGetURLWithTTL
	t.Cleanup(func() { presignArtifactGetURLWithTTL = oldPresign })
	var gotTTL time.Duration
	presignArtifactGetURLWithTTL = func(_ context.Context, artifact *models.RootfsArtifact, ttl time.Duration) (string, error) {
		gotTTL = ttl
		if artifact.ArtifactID != "artifact-s3" {
			t.Fatalf("artifact id = %q", artifact.ArtifactID)
		}
		return "https://objects.example/new-signature", nil
	}
	now := time.Unix(1_800_000_000, 0)
	ticket, err := buildArtifactDownloadTicket(context.Background(), &models.RootfsArtifact{
		ArtifactID:     "artifact-s3",
		Generation:     2,
		StorageBackend: "s3",
		ArtifactURL:    "https://objects.example/stale-signature",
		Ext4SizeBytes:  42,
		Ext4SHA256:     "sha",
	}, "node-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if gotTTL != 2*time.Hour || ticket.Method != "GET" || len(ticket.Headers) != 0 ||
		ticket.Source != artifactticket.SourceS3 ||
		ticket.URL != "https://objects.example/new-signature" || ticket.Token != "" {
		t.Fatalf("ticket = %+v, presign ttl = %s", ticket, gotTTL)
	}
}

func TestBuildArtifactDownloadTicketDoesNotFallBackToStoredS3URL(t *testing.T) {
	oldPresign := presignArtifactGetURLWithTTL
	t.Cleanup(func() { presignArtifactGetURLWithTTL = oldPresign })
	wantErr := errors.New("signer unavailable")
	presignArtifactGetURLWithTTL = func(context.Context, *models.RootfsArtifact, time.Duration) (string, error) {
		return "", wantErr
	}
	_, err := buildArtifactDownloadTicket(context.Background(), &models.RootfsArtifact{
		ArtifactID:     "artifact-s3",
		Generation:     1,
		StorageBackend: "s3",
		ArtifactURL:    "https://objects.example/stale-signature",
	}, "node-1", time.Now())
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want signer error", err)
	}
}

func TestEnsureArtifactStableLocatorBackfillsLegacyRow(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:artifact-stable-locator?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.RootfsArtifact{}); err != nil {
		t.Fatal(err)
	}
	artifact := &models.RootfsArtifact{
		ArtifactID: "artifact-legacy",
		Ext4Path:   "/var/lib/cube/artifact-legacy.ext4",
		Status:     ArtifactStatusReady,
	}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://tc.internal:8090")

	if err := ensureArtifactStableLocator(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	var got models.RootfsArtifact
	if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Generation != 1 ||
		got.OwnerTCDownloadBase != "http://tc.internal:8090" ||
		got.StorageRef != "file:///var/lib/cube/artifact-legacy.ext4" {
		t.Fatalf("backfilled artifact = %+v", got)
	}
}

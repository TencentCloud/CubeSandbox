// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package httpservice

// This file is a cross-package Go integration test for the artifact
// ticket-download protocol described in
// docs/dev/template-reliability-test-guide.md §6/§11 (D02/D04/E02): Master
// issues a short-lived, node- and generation-bound download ticket, and
// Cubelet uses it to fetch the artifact bytes directly from the owning
// TemplateCenter — never through Master, and never using the TC-wide
// component/callback secret.
//
// This is *not* the end-to-end test for that protocol: it runs CubeMaster's
// and CubeTemplateCenter's real functions in a single Go test binary and
// process, with no real network hop between separate services, no real
// Cubelet, and no real sandbox boot. The actual E2E coverage for the
// artifact-serving path lives in Python against a real running stack:
// tests/e2e/sdk_compat/cases/templates/test_new_template_sandbox_lifecycle.py
// builds a real template, boots a real sandbox pinned to that exact
// template_id, and proves it runs a real command and file roundtrip — i.e.
// it proves the artifact this ticket protocol serves is actually bootable.
//
// What this file adds on top of that: unlike
// TestNodeArtifactDownloadHandlerUsesPerNodeTokenNotComponentSecret in
// artifact_download_test.go (which stubs verifyNodeArtifactDownload), this
// test exercises the *real* production functions on both sides of the
// authorization hop — CubeMaster's templatecenter.IssueArtifactDownloadTicket
// and templatecenter.VerifyArtifactDownloadToken — wired to the same
// throwaway SQLite DB via the exported SetArtifactDownloadStoreForTest seam,
// to prove the two real code paths agree on forged/wrong-node/revoked
// tickets. The Python E2E suite above cannot reach into CubeMaster's
// internal ticket-issuance endpoint or component secrets (and must not
// hardcode them per the test guide's credential-handling rule), so these
// authorization edge cases are only practically testable here.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
)

// ticketDownloadFixture wires a real SQLite-backed templatecenter store, a
// real TC node-download HTTP server, and a READY fs-backed artifact with one
// DISTRIBUTING replica on "node-1". It is shared by every sub-scenario below
// so each only has to vary the one condition it is testing.
type ticketDownloadFixture struct {
	artifact *models.RootfsArtifact
	replica  *models.TemplateReplica
	db       *gorm.DB
	tcServer *httptest.Server
	body     []byte
}

func newTicketDownloadFixture(t *testing.T) *ticketDownloadFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

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

	const nodeID = "node-1"
	restore := templatecenter.SetArtifactDownloadStoreForTest(db, func(id string) (*node.Node, bool) {
		return &node.Node{InsID: id}, id == nodeID
	})
	t.Cleanup(restore)
	t.Setenv(constants.TemplateCallbackTokenEnv, "integration-control-plane-secret-01234567890123")

	// Lay the bytes out exactly as blobstore/fs does (<root>/_data/<key>),
	// because fs-backed artifacts are resolved via StorageRef + ObjectKey,
	// not Ext4Path.
	body := []byte("rootfs-bytes-for-integration-ticket-download")
	fsRoot := t.TempDir()
	const objectKey = "templates/integration-artifact-1.ext4"
	objectPath := filepath.Join(fsRoot, "_data", filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/internal/artifacts/:artifact_id/ext4", nodeArtifactDownloadHandler)
	router.HEAD("/internal/artifacts/:artifact_id/ext4", nodeArtifactDownloadHandler)
	tcServer := httptest.NewServer(router)
	t.Cleanup(tcServer.Close)

	artifact := &models.RootfsArtifact{
		ArtifactID:          "integration-artifact-1",
		Generation:          3,
		Status:              templatecenter.ArtifactStatusReady,
		StorageBackend:      "fs",
		StorageRef:          "file://" + fsRoot,
		ObjectKey:           objectKey,
		Ext4SizeBytes:       int64(len(body)),
		Ext4SHA256:          "integration-sha256-placeholder",
		OwnerTCDownloadBase: tcServer.URL,
		MasterNodeIP:        "http://master.example:8089",
	}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	replica := &models.TemplateReplica{
		TemplateID: "integration-template-1",
		ArtifactID: artifact.ArtifactID,
		NodeID:     nodeID,
		Phase:      templatecenter.ReplicaPhaseDistributing,
	}
	if err := db.Create(replica).Error; err != nil {
		t.Fatal(err)
	}

	return &ticketDownloadFixture{artifact: artifact, replica: replica, db: db, tcServer: tcServer, body: body}
}

// issueTicket drives the real Master-side issuance path: a refresh
// capability minted for nodeID, exchanged for a download ticket.
func (f *ticketDownloadFixture) issueTicket(t *testing.T, nodeID string) *artifactticket.Ticket {
	t.Helper()
	ctx := context.Background()
	refresh, err := templatecenter.BuildArtifactDownloadRefreshToken(ctx, f.artifact, nodeID, time.Now())
	if err != nil {
		t.Fatalf("mint refresh capability: %v", err)
	}
	ticket, err := templatecenter.IssueArtifactDownloadTicket(ctx, f.artifact.ArtifactID, nodeID, refresh)
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	return ticket
}

// fetch replays exactly what Cubelet sends on the wire for a TC-sourced
// ticket: the method, URL and headers straight out of the ticket, nothing
// else added (in particular, no component/callback secret).
func (f *ticketDownloadFixture) fetch(t *testing.T, ticket *artifactticket.Ticket) *http.Response {
	t.Helper()
	req, err := http.NewRequest(ticket.Method, ticket.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range ticket.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// E02 (docs/dev/template-reliability-test-guide.md §11): a validly issued
// ticket downloads the real bytes straight from TC, never through Master,
// and without presenting the TC-wide component secret.
func TestTicketDownloadIntegration_ValidTicketServesRealBytes(t *testing.T) {
	f := newTicketDownloadFixture(t)
	ticket := f.issueTicket(t, "node-1")
	if ticket.Source != artifactticket.SourceTemplateCenter {
		t.Fatalf("source = %q, want TemplateCenter (fs artifacts never go through S3)", ticket.Source)
	}
	resp := f.fetch(t, ticket)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(f.body) {
		t.Fatalf("body = %q, want %q", buf, f.body)
	}
}

// D04: a forged token (signed with the right algorithm but not by Master's
// real signing key derivation) must be rejected by TC's real verification,
// not just by a stub.
func TestTicketDownloadIntegration_ForgedTokenRejected(t *testing.T) {
	f := newTicketDownloadFixture(t)
	ticket := f.issueTicket(t, "node-1")
	ticket.Headers[artifactticket.DownloadTokenHeader] = ticket.Headers[artifactticket.DownloadTokenHeader] + "-tampered"
	resp := f.fetch(t, ticket)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for forged token", resp.StatusCode)
	}
}

// D04: a ticket minted for one node must not authorize a different node
// that presents it (e.g. a copy/replay from another host).
func TestTicketDownloadIntegration_WrongNodeRejected(t *testing.T) {
	f := newTicketDownloadFixture(t)
	ticket := f.issueTicket(t, "node-1")
	ticket.Headers[artifactticket.DownloadNodeIDHeader] = "node-2"
	resp := f.fetch(t, ticket)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for wrong node id", resp.StatusCode)
	}
}

// D04: once the control plane revokes the node's replica placement (e.g.
// the template was redistributed elsewhere), an already-issued ticket must
// stop working on the very next request — authorization is re-checked on
// every download, not cached from issuance time.
func TestTicketDownloadIntegration_RevokedReplicaRejectsAlreadyIssuedTicket(t *testing.T) {
	f := newTicketDownloadFixture(t)
	ticket := f.issueTicket(t, "node-1")
	if resp := f.fetch(t, ticket); resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-revocation status = %d, want 200", resp.StatusCode)
	}
	if err := f.db.Delete(f.replica).Error; err != nil {
		t.Fatal(err)
	}
	resp := f.fetch(t, ticket)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post-revocation status = %d, want 403", resp.StatusCode)
	}
}

// E02: the component/callback secret that CubeMaster<->TC service calls use
// must not be independently sufficient on the node-facing route, even when
// presented together with other node headers but without a valid node
// token. This complements TestNodeArtifactDownloadHandlerUsesPerNodeTokenNotComponentSecret
// by driving it through the real ticket issuance path instead of a literal
// header fixture.
func TestTicketDownloadIntegration_ComponentSecretAloneInsufficient(t *testing.T) {
	f := newTicketDownloadFixture(t)
	ticket := f.issueTicket(t, "node-1")
	// Deliberately send only the component secret plus the right node id,
	// withholding the per-node download token.
	req, err := http.NewRequest(http.MethodGet, ticket.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(artifactticket.DownloadNodeIDHeader, ticket.Headers[artifactticket.DownloadNodeIDHeader])
	req.Header.Set(constants.TemplateCallbackTokenHeader, "integration-control-plane-secret-01234567890123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing node token, component secret must not substitute)", resp.StatusCode)
	}
}

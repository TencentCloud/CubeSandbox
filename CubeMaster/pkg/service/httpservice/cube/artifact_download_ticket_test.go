// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestArtifactDownloadTicketRouteIssuesDirectTemplateCenterTicket drives the
// real Master ticket route against a throwaway store and asserts the ticket
// sends Cubelet straight to the owning TemplateCenter: Master only issues
// tickets and never appears as the byte source.
func TestArtifactDownloadTicketRouteIssuesDirectTemplateCenterTicket(t *testing.T) {
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
	t.Cleanup(templatecenter.SetArtifactDownloadStoreForTest(db, func(id string) (*node.Node, bool) {
		return &node.Node{InsID: id}, id == nodeID
	}))
	t.Setenv(constants.TemplateCallbackTokenEnv, strings.Repeat("ticket-route-control-secret-", 2))

	artifact := &models.RootfsArtifact{
		ArtifactID:          "artifact-route-1",
		Generation:          1,
		Status:              templatecenter.ArtifactStatusReady,
		StorageBackend:      "fs",
		StorageRef:          "file:///data/cube-artifacts",
		Ext4Path:            "/data/cube-artifacts/artifact-route-1.ext4",
		Ext4SizeBytes:       4,
		Ext4SHA256:          "sha",
		OwnerTCDownloadBase: "http://tc.internal:8090",
		MasterNodeIP:        "http://master.internal:8089",
	}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.TemplateReplica{TemplateID: "tpl-route", ArtifactID: artifact.ArtifactID,
		NodeID: nodeID, Phase: templatecenter.ReplicaPhaseDistributing}).Error; err != nil {
		t.Fatal(err)
	}
	refresh, err := templatecenter.BuildArtifactDownloadRefreshToken(context.Background(), artifact, nodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	RegisterInternalTemplateRoutes(router.Group(""))
	request := func(artifactID, node, refreshToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/internal/template/artifacts/"+artifactID+"/download-ticket", nil)
		if node != "" {
			req.Header.Set(constants.ArtifactDownloadNodeIDHeader, node)
		}
		if refreshToken != "" {
			req.Header.Set(constants.ArtifactDownloadRefreshHeader, refreshToken)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	ok := request(artifact.ArtifactID, nodeID, refresh)
	if ok.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", ok.Code, ok.Body.String())
	}
	if got := ok.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var ticket artifactticket.Ticket
	if err := json.Unmarshal(ok.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Source != artifactticket.SourceTemplateCenter ||
		ticket.URL != "http://tc.internal:8090/internal/artifacts/artifact-route-1/ext4" {
		t.Fatalf("ticket source/url = %q %q, want direct TC download", ticket.Source, ticket.URL)
	}
	if strings.Contains(ticket.URL, "master.internal") || strings.Contains(ticket.URL, "/internal/template/") {
		t.Fatalf("ticket points bytes at CubeMaster: %q", ticket.URL)
	}

	if got := request(artifact.ArtifactID, "", refresh).Code; got != http.StatusBadRequest {
		t.Fatalf("missing node status = %d, want 400", got)
	}
	if got := request(artifact.ArtifactID, nodeID, refresh+"x").Code; got != http.StatusForbidden {
		t.Fatalf("forged refresh status = %d, want 403", got)
	}
	if got := request(artifact.ArtifactID, "node-2", refresh).Code; got != http.StatusForbidden {
		t.Fatalf("wrong node status = %d, want 403", got)
	}
}

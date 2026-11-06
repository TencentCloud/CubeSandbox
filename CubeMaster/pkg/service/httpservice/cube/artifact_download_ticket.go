// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"gorm.io/gorm"
)

var artifactDownloadIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// handleArtifactDownloadTicket is called by Cubelet before it downloads an
// artifact directly from S3 or the owning TemplateCenter. Replica
// authorization is checked on every refresh, so an expired capability
// cannot be renewed after placement is removed.
func handleArtifactDownloadTicket(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	nodeID := strings.TrimSpace(c.GetHeader(constants.ArtifactDownloadNodeIDHeader))
	if !artifactDownloadIDPattern.MatchString(artifactID) || !artifactDownloadIDPattern.MatchString(nodeID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "valid artifact_id and X-Cube-Node-ID are required"})
		return
	}
	ticket, err := templatecenter.IssueArtifactDownloadTicket(
		c.Request.Context(),
		artifactID,
		nodeID,
		c.GetHeader(constants.ArtifactDownloadRefreshHeader),
	)
	if err == nil {
		c.JSON(http.StatusOK, ticket)
		return
	}
	switch {
	case errors.Is(err, templatecenter.ErrArtifactDownloadUnauthorized):
		c.JSON(http.StatusForbidden, gin.H{"error": "artifact download is not authorized"})
	case errors.Is(err, templatecenter.ErrArtifactDownloadUnconfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "artifact not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue artifact download ticket failed"})
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/build"
)

// buildExecutor is the process-wide build owner. Set by RegisterInternalRoutes'
// caller (main) so the handler shares the executor the app shuts down with.
var buildExecutor *build.Executor

// artifactDeleter is the process-wide artifact delete owner. Set by main so
// the delete endpoint shares the deleter the reconciler's backstop sweep uses.
var artifactDeleter *build.ArtifactDeleter

// SetBuildExecutor installs the executor the build endpoint submits to. Called
// once at startup before routes are served.
func SetBuildExecutor(e *build.Executor) {
	buildExecutor = e
}

// SetArtifactDeleter installs the deleter the artifact-delete endpoint uses.
// Called once at startup before routes are served.
func SetArtifactDeleter(d *build.ArtifactDeleter) {
	artifactDeleter = d
}

// handleBuildSubmit receives a build job from CubeMaster and starts the build.
// TC reports status back to CubeMaster via POST /internal/template/jobs/:job_id/status.
func handleBuildSubmit(c *gin.Context) {
	var req BuildJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	if buildExecutor == nil {
		// Misconfiguration: the app should always install an executor. Fail
		// closed rather than run an untracked, unkillable build.
		log.G(c.Request.Context()).Errorf("build submit rejected: no executor installed, job_id=%s", req.JobID)
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "build executor not initialized"})
		return
	}

	err := buildExecutor.Submit(req.JobID, req.Request, req.DownloadBaseURL, req.EnvdSHA256, req.EnvdData)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, build.ErrBuildJobNotFound):
			// 404 also stops CubeMaster's reporter/forwarder from retrying a job
			// that will never exist.
			status = http.StatusNotFound
		case errors.Is(err, build.ErrBuildJobDuplicate):
			status = http.StatusConflict
		case errors.Is(err, build.ErrBuildConcurrencyLimit):
			status = http.StatusTooManyRequests
		}
		log.G(c.Request.Context()).Warnf("build submit rejected: job_id=%s err=%v", req.JobID, err)
		c.JSON(status, ErrorResponse{Error: err.Error()})
		return
	}

	log.G(c.Request.Context()).Infof("received build job: job_id=%s image=%s", req.JobID, req.Request.SourceImageRef)
	c.JSON(http.StatusOK, BuildJobResponse{Status: "accepted", JobID: req.JobID})
}

// handleArtifactDelete removes an artifact's data (S3 object + local ext4)
// and its row. Called by CubeMaster after it has marked the artifact row
// CLEANUP_PENDING and removed its own template/job rows. Idempotent.
func handleArtifactDelete(c *gin.Context) {
	var req ArtifactDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if artifactDeleter == nil {
		log.G(c.Request.Context()).Errorf("artifact delete rejected: no deleter installed, artifact_id=%s", req.ArtifactID)
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "artifact deleter not initialized"})
		return
	}
	if err := artifactDeleter.Delete(c.Request.Context(), req.ArtifactID); err != nil {
		log.G(c.Request.Context()).Warnf("artifact delete fail: artifact_id=%s err=%v", req.ArtifactID, err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	log.G(c.Request.Context()).Infof("deleted artifact: artifact_id=%s", req.ArtifactID)
	c.JSON(http.StatusOK, ArtifactDeleteResponse{Status: "deleted", ArtifactID: req.ArtifactID})
}

// RegisterInternalRoutes registers TC's internal API routes.
func RegisterInternalRoutes(g *gin.RouterGroup) {
	g.POST("/tc/api/v1/build", handleBuildSubmit)
	g.POST("/tc/api/v1/artifact/delete", handleArtifactDelete)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/build"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
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
		case errors.Is(err, build.ErrBuildJobRequestMismatch):
			// The submitted payload diverges from the request_json CubeMaster
			// persisted for this job; building it would register an artifact
			// the job never asked for.
			status = http.StatusBadRequest
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

// handleArtifactUpload ingests a local ext4 uploaded by CubeMaster and stores
// it into CubeTemplateCenter's own artifact store.
//
// The body is consumed with a single streaming MultipartReader pass rather
// than c.PostForm/c.FormFile: those parse the whole form first, buffering up
// to MaxMultipartMemory in RAM and spooling the rest into os.TempDir() -- for
// GB-scale ext4 artifacts that means a full extra copy in /tmp (often tmpfs,
// i.e. RAM) before a single byte reaches the artifact store.
//
// artifact_id must arrive BEFORE the file part (tcclient writes it first):
// the id shapes the destination path, so it is validated before any payload
// byte touches the filesystem.
// maxArtifactUploadBytes caps the upload body so a stolen callback token (or
// a buggy Master) cannot fill TC's disk. Ext4 artifacts are single-digit GiB
// in practice; 32 GiB leaves ample headroom while still failing closed.
const maxArtifactUploadBytes = 32 << 30

func handleArtifactUpload(c *gin.Context) {
	ctx := c.Request.Context()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxArtifactUploadBytes)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("read multipart body: %v", err)})
		return
	}

	artifactID := ""
	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("read multipart part: %v", nextErr)})
			return
		}
		switch part.FormName() {
		case "artifact_id":
			data, readErr := io.ReadAll(io.LimitReader(part, 1024))
			part.Close()
			if readErr != nil {
				c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("read artifact_id field: %v", readErr)})
				return
			}
			artifactID = strings.TrimSpace(string(data))
			if !image.ValidArtifactID(artifactID) {
				c.JSON(http.StatusBadRequest, ErrorResponse{Error: "artifact_id has an invalid shape"})
				return
			}
		case "file":
			if artifactID == "" {
				part.Close()
				c.JSON(http.StatusBadRequest, ErrorResponse{Error: "artifact_id field must precede the file part"})
				return
			}
			handleArtifactUploadFilePart(c, ctx, artifactID, part)
			part.Close()
			return
		default:
			part.Close()
		}
	}
	if artifactID == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "artifact_id is required"})
		return
	}
	c.JSON(http.StatusBadRequest, ErrorResponse{Error: "file is required"})
}

// handleArtifactUploadFilePart streams one validated file part into the
// artifact store, deduping on identical content already in place.
func handleArtifactUploadFilePart(c *gin.Context, ctx context.Context, artifactID string, reader io.Reader) {
	storeDir, err := image.ResolveArtifactStoreDir(ctx, artifactID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("resolve artifact store dir: %v", err)})
		return
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("create artifact store dir: %v", err)})
		return
	}

	tmpFile, err := os.CreateTemp(storeDir, artifactID+".upload-*.tmp")
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("create temp file: %v", err)})
		return
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		_ = tmpFile.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmpFile, hasher), reader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("save uploaded file: %v", err)})
		return
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if size <= 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "uploaded file is empty"})
		return
	}
	if err := tmpFile.Close(); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("close temp file: %v", err)})
		return
	}

	dstPath := filepath.Join(storeDir, artifactID+".ext4")
	if st, statErr := os.Stat(dstPath); statErr == nil && st.Mode().IsRegular() {
		dstSha, shaErr := fileSHA256(dstPath)
		if shaErr == nil && st.Size() == size && dstSha == sha {
			cleanupTmp = true
			c.JSON(http.StatusOK, ArtifactUploadResponse{
				Status:        "reused",
				ArtifactID:    artifactID,
				Ext4Path:      dstPath,
				Ext4SHA256:    dstSha,
				Ext4SizeBytes: st.Size(),
			})
			return
		}
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("promote uploaded artifact file: %v", err)})
		return
	}
	cleanupTmp = false
	if err := os.Chmod(dstPath, 0o644); err != nil {
		log.G(ctx).Warnf("artifact upload: chmod %s failed: %v", dstPath, err)
	}

	c.JSON(http.StatusOK, ArtifactUploadResponse{
		Status:        "uploaded",
		ArtifactID:    artifactID,
		Ext4Path:      dstPath,
		Ext4SHA256:    sha,
		Ext4SizeBytes: size,
	})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sharedTokenWarnOnce rate-limits the "unauthenticated endpoint" warning.
var sharedTokenWarnOnce sync.Once

// internalAPIAuthorized gates the internal build/artifact endpoints on the
// same shared secret TC attaches to its build-status callbacks
// (constants.TemplateCallbackTokenEnv / constants.TemplateCallbackTokenHeader).
// These endpoints can start builds and delete artifacts, so they must not
// stay anonymous to anything that can reach the HTTP listener.
//
// Fail closed: the chart, one-click installer and Terraform all generate the
// secret on both sides, so an unset token is a misconfiguration, not a
// supported mode. Allowing requests through would leave build submit and
// artifact delete anonymous.
func internalAPIAuthorized(c *gin.Context) bool {
	want := strings.TrimSpace(tcconfig.CallbackToken())
	if want == "" {
		return false
	}
	got := strings.TrimSpace(c.GetHeader(constants.TemplateCallbackTokenHeader))
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// internalAuthMiddleware rejects unauthenticated calls to the internal API.
//
// Token unset fails CLOSED: the chart, one-click installer and Terraform all
// generate the shared secret on both sides, so an unset token is a
// misconfiguration. The only exception is the explicit dev opt-in
// (constants.TemplateCallbackInsecureNoTokenEnv, single-binary local runs
// only) — matching CubeMaster's callback gate.
func internalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.TrimSpace(tcconfig.CallbackToken()) == "" {
			if strings.EqualFold(strings.TrimSpace(os.Getenv(constants.TemplateCallbackInsecureNoTokenEnv)), "true") {
				sharedTokenWarnOnce.Do(func() {
					log.G(c.Request.Context()).Warnf(
						"%s is not set and %s=true: TC internal API (build submit / artifact delete) is UNAUTHENTICATED (single-binary dev mode); never enable this in a deployed environment",
						constants.TemplateCallbackTokenEnv, constants.TemplateCallbackInsecureNoTokenEnv)
				})
				c.Next()
				return
			}
			sharedTokenWarnOnce.Do(func() {
				log.G(c.Request.Context()).Errorf(
					"%s is not set: TC internal API (build submit / artifact delete) refuses all calls; set it on both CubeMaster and CubeTemplateCenter (or %s=true for single-binary local dev)",
					constants.TemplateCallbackTokenEnv, constants.TemplateCallbackInsecureNoTokenEnv)
			})
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{Error: constants.TemplateCallbackTokenEnv + " is not configured on template center"})
			return
		}
		if !internalAPIAuthorized(c) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid or missing shared token"})
			return
		}
		c.Next()
	}
}

// RegisterInternalRoutes registers TC's internal API routes. All of them sit
// behind the shared-token middleware: a forged build submit would burn image
// pulls and register attacker-controlled rootfs content, and a forged
// artifact delete destroys template data.
func RegisterInternalRoutes(g *gin.RouterGroup) {
	internal := g.Group("/tc/api/v1", internalAuthMiddleware())
	internal.POST("/build", handleBuildSubmit)
	internal.POST("/artifact/delete", handleArtifactDelete)
	internal.POST("/artifact/upload", handleArtifactUpload)
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

var redoTemplateFromImageFn = templatecenter.SubmitRedoTemplateFromImage

// getRootfsArtifactForRedirectFn is the seam redirectToS3Artifact uses to look
// up the artifact row. Declared as a variable so tests can stub it without a
// database (mirrors redoTemplateFromImageFn above).
var getRootfsArtifactForRedirectFn = templatecenter.GetRootfsArtifactForRedirect

func createTemplateFromImageGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, createTemplateFromImage(c.Request, rt))
}

func getTemplateFromImageGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, getTemplateFromImage(c.Request, rt))
}

func handleRedoTemplateAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &types.RedoTemplateFromImageReq{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RequestID = req.RequestID
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":  req.RequestID,
		"Action":     "RedoTemplate",
		"TemplateID": req.TemplateID,
	}))
	// CubeMaster no longer builds templates in-process, including redo full
	// rebuilds. The redo job is persisted here and forwarded to
	// CubeTemplateCenter for the actual build work.
	job, err := redoTemplateFromImageFn(ctx, req, requestBaseURL(c.Request))
	if err != nil {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	// Redo may be a full rebuild (needs TC) or a redistribution-only (no build).
	// SubmitRedoTemplateFromImage already decided: full-rebuild jobs stay
	// PENDING and must be forwarded to TC; redistribution-only jobs run locally.
	if job != nil && templatecenter.RedoNeedsFullRebuild(c.Request.Context(), job.JobID) {
		go forwardRedoBuildJobToTemplateCenter(job.JobID, requestBaseURL(c.Request))
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &types.CreateTemplateFromImageRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	})
}

func createTemplateFromImage(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req, envdPayload, err := parseCreateTemplateFromImageRequest(r)
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	rt.RequestID = req.RequestID
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
		"Action":       "CreateTemplateFromImage",
		"TemplateID":   req.TemplateID,
	}))
	// CubeMaster no longer builds templates in-process. All template builds are
	// forwarded to the standalone CubeTemplateCenter process.
	//
	// Forward the NORMALIZED request (the exact object persisted into the
	// job's request_json snapshot), not the raw client request: TC binds the
	// submitted payload to that snapshot, and the raw request differs from it
	// (client requests carry no generated template_id / defaults), so
	// forwarding `req` here would be rejected as a payload mismatch.
	job, normalizedReq, err := templatecenter.SubmitTemplateFromImageWithoutBuild(ctx, req, requestBaseURL(r))
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	// Only a job still awaiting a build (PENDING) needs forwarding to TC.
	// SubmitTemplateFromImageWithoutBuild can also return an existing job
	// that is already RUNNING (an identical in-flight request was reused) --
	// forwarding that again would resubmit a job TC already has. TC itself
	// only ever accepts PENDING/RUNNING build jobs it created, so a
	// non-PENDING job forwarded here would 404 and get wrongly marked FAILED.
	if job != nil && job.Status == templatecenter.JobStatusPending {
		go forwardBuildJobToTemplateCenter(job.JobID, normalizedReq, requestBaseURL(r), envdPayload)
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &types.CreateTemplateFromImageRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	}
}

func getTemplateFromImage(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	jobID := strings.TrimSpace(r.URL.Query().Get("job_id"))
	if jobID == "" {
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "job_id is required",
			},
		}
	}
	job, err := templatecenter.GetTemplateImageJobInfo(r.Context(), jobID)
	if err != nil {
		code := templateImageJobErrorCode(err)
		if rt != nil {
			rt.RetCode = int64(code)
		}
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		}
	}
	if rt != nil {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
	}
	return &types.CreateTemplateFromImageRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	}
}

// templateImageJobErrorCode maps a build-job lookup error to a ret code.
//
// Shared by every handler that reads a job so they cannot disagree on what an
// absent job means. Anything unrecognised stays MasterInternalError: guessing
// a client-side code for an unknown failure would hide real server faults.
func templateImageJobErrorCode(err error) int {
	switch {
	case err == nil:
		return int(errorcode.ErrorCode_Success)
	case errors.Is(err, templatecenter.ErrTemplateImageJobNotFound):
		// "no such job" is a client-side fact, not a server fault. Returning
		// MasterInternalError here made every probe for a missing job look like
		// CubeMaster had broken.
		return int(errorcode.ErrorCode_NotFound)
	case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
		return int(errorcode.ErrorCode_DBError)
	default:
		return int(errorcode.ErrorCode_MasterInternalError)
	}
}

// openTemplateArtifactForDownload resolves, opens, and stats the template
// rootfs artifact identified by the artifact_id/token query params and writes
// the common response headers (Content-Type/Length, ETag, X-Cube-Artifact-Id).
// On error it writes the API error response and returns ok=false. On success
// the caller owns file (must Close).
func openTemplateArtifactForDownload(c *gin.Context) (name string, file *os.File, stat os.FileInfo, ok bool) {
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	token := strings.TrimSpace(c.Query("token"))
	record, f, err := templatecenter.OpenRootfsArtifact(c.Request.Context(), artifactID, token)
	if err != nil {
		common.WriteAPI(c, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_NotFound),
				RetMsg:  err.Error(),
			},
		})
		return "", nil, nil, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		common.WriteAPI(c, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterInternalError),
				RetMsg:  err.Error(),
			},
		})
		return "", nil, nil, false
	}
	c.Writer.Header().Set("Content-Type", "application/octet-stream")
	c.Writer.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	c.Writer.Header().Set("ETag", record.Ext4SHA256)
	c.Writer.Header().Set("X-Cube-Artifact-Id", record.ArtifactID)
	return filepath.Base(record.Ext4Path), f, st, true
}

// artifactProxyHTTPClient has no total Timeout on purpose: artifact streams
// are GB-scale, so a whole-request deadline would abort healthy downloads.
// ResponseHeaderTimeout bounds the only phase that can hang silently (waiting
// on a stalled S3/TC response), after which the stream is driven by the
// downstream client's disconnect via the request context.
var artifactProxyHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

func downloadTemplateArtifactGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())

	// S3-backed artifacts are proxied through this endpoint rather than 302
	// redirected to the object store. Nodes therefore only need reachability to
	// CubeMaster's public address; Master/TC absorb any S3 endpoint topology.
	if handled, ok := proxyS3Artifact(c); handled {
		rt.RetCode = artifactProxyRetCode(ok)
		return
	}

	name, file, stat, ok := openTemplateArtifactForDownload(c)
	if !ok {
		return
	}
	defer file.Close()
	http.ServeContent(c.Writer, c.Request, name, stat.ModTime(), file)
	rt.RetCode = int64(errorcode.ErrorCode_Success)
}

func headTemplateArtifactGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())

	// HEAD follows the same S3 proxy-vs-local split as GET so servability probes
	// exercise the exact node-facing download path.
	if handled, ok := proxyS3Artifact(c); handled {
		rt.RetCode = artifactProxyRetCode(ok)
		return
	}

	_, file, _, ok := openTemplateArtifactForDownload(c)
	if !ok {
		return
	}
	file.Close()
	rt.RetCode = int64(errorcode.ErrorCode_Success)
}

// artifactProxyRetCode keeps the request log honest: an upstream proxy failure
// wrote a 502 to the client and must not be recorded as a success.
func artifactProxyRetCode(ok bool) int64 {
	if ok {
		return int64(errorcode.ErrorCode_Success)
	}
	return int64(errorcode.ErrorCode_MasterInternalError)
}

// proxyS3Artifact streams an S3-backed artifact through the current handler
// instead of redirecting the caller to the presigned URL. handled=false means
// the artifact is local-disk or the row/token lookup failed, so the caller
// should fall through to the local-file path which writes its own error
// response. Once handled=true a response has been written; ok reports whether
// the upstream fetch/stream succeeded (false -> we wrote 502, or the stream
// broke mid-flight).
func proxyS3Artifact(c *gin.Context) (handled bool, ok bool) {
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	token := strings.TrimSpace(c.Query("token"))
	if artifactID == "" {
		return false, false
	}
	record, err := getRootfsArtifactForRedirectFn(c.Request.Context(), artifactID, token)
	if err != nil || record == nil {
		return false, false
	}
	if record.ArtifactURL == "" {
		return false, false
	}
	downloadURL := templatecenter.ArtifactDownloadURL(c.Request.Context(), record)
	if downloadURL == "" {
		log.G(c.Request.Context()).Warnf("artifact proxy: empty download url for s3-backed artifact %s", record.ArtifactID)
		c.AbortWithStatus(http.StatusBadGateway)
		return true, false
	}
	// The presigned URL is signed for GET (SigV4 covers the method), so a HEAD
	// probe must be proxied as a GET whose body we simply do not forward --
	// forwarding HEAD verbatim gets a 403 from S3/MinIO.
	upstreamMethod := c.Request.Method
	if upstreamMethod == http.MethodHead {
		upstreamMethod = http.MethodGet
	}
	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), upstreamMethod, downloadURL, nil)
	if err != nil {
		log.G(c.Request.Context()).Warnf("artifact proxy: build upstream request for %s failed: %v", record.ArtifactID, err)
		c.AbortWithStatus(http.StatusBadGateway)
		return true, false
	}
	for _, key := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
		if value := strings.TrimSpace(c.Request.Header.Get(key)); value != "" {
			upstreamReq.Header.Set(key, value)
		}
	}
	resp, err := artifactProxyHTTPClient.Do(upstreamReq)
	if err != nil {
		log.G(c.Request.Context()).Warnf("artifact proxy: fetch %s failed: %v", record.ArtifactID, err)
		c.AbortWithStatus(http.StatusBadGateway)
		return true, false
	}
	defer resp.Body.Close()
	copyArtifactProxyHeaders(c.Writer.Header(), resp.Header)
	c.Writer.Header().Set("X-Cube-Artifact-Id", record.ArtifactID)
	c.Writer.Header().Set("ETag", record.Ext4SHA256)
	c.Status(resp.StatusCode)
	if c.Request.Method == http.MethodHead {
		// Drain nothing: the body is discarded so the connection can reuse or
		// close promptly; the caller only wanted headers.
		return true, resp.StatusCode < http.StatusBadRequest
	}
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		log.G(c.Request.Context()).Warnf("artifact proxy: stream %s failed: %v", record.ArtifactID, err)
		return true, false
	}
	return true, resp.StatusCode < http.StatusBadRequest
}

func copyArtifactProxyHeaders(dst, src http.Header) {
	for _, key := range []string{"Accept-Ranges", "Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Length", "Content-Range", "Content-Type", "Last-Modified"} {
		values := src.Values(key)
		if len(values) == 0 {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func handleRootfsArtifactAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	if artifactID == "" {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "artifact_id is required",
			},
		})
		return
	}
	info, err := templatecenter.GetRootfsArtifactInfo(c.Request.Context(), artifactID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			code = int(errorcode.ErrorCode_NotFound)
		}
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &types.CreateTemplateFromImageRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: &types.TemplateImageJobInfo{
			ArtifactID:     info.ArtifactID,
			ArtifactStatus: info.Status,
			Artifact:       info,
		},
	})
}

// requestBaseURL returns the base URL other components must use to reach this
// CubeMaster.
//
// The source is intentionally simple and explicit: prefer CUBE_MASTER_ADDR (or
// common.master_addr), otherwise fall back to the current request's Host/addr.
// Do NOT guess another address from local interfaces here: the deployment must
// decide which address other components should use.
func requestBaseURL(r *http.Request) string {
	configured := ""
	if cfg := config.GetConfig(); cfg != nil {
		configured = strings.TrimSpace(cfg.MasterAddr())
	}
	if configured != "" {
		return templatecenter.NormalizeBaseURL(configured)
	}
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if host := strings.TrimSpace(r.Host); host != "" {
		return scheme + "://" + host
	}
	return ""
}

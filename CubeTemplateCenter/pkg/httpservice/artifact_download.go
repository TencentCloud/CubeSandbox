// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package httpservice

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
	"gorm.io/gorm"
)

var directArtifactIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var errArtifactDownloadStoreUnavailable = errors.New("artifact download store is unavailable")

// verifyNodeArtifactDownload is swapped in tests.
var verifyNodeArtifactDownload = templatecenter.VerifyArtifactDownloadToken

var loadDirectDownloadArtifact = func(c *gin.Context, artifactID string) (*models.RootfsArtifact, error) {
	db := templatecenter.GetDB()
	if db == nil {
		return nil, errArtifactDownloadStoreUnavailable
	}
	var artifact models.RootfsArtifact
	err := db.WithContext(c.Request.Context()).Unscoped().
		Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND deleted_at IS NULL", artifactID).
		First(&artifact).Error
	return &artifact, err
}

func artifactDownloadHandler(c *gin.Context) {
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	if !directArtifactIDPattern.MatchString(artifactID) {
		c.Status(http.StatusBadRequest)
		return
	}

	artifact, err := loadDirectDownloadArtifact(c, artifactID)
	if errors.Is(err, errArtifactDownloadStoreUnavailable) {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	if artifact.Status != templatecenter.ArtifactStatusReady {
		c.Status(http.StatusGone)
		return
	}
	serveArtifactFile(c, artifact)
}

// nodeArtifactDownloadHandler serves direct-mode fs-backed artifact bytes to
// Cubelet. It sits on a route with NO shared component-token middleware (see
// registerRoutes in server.go): authentication here is the short-lived,
// node- and generation-bound download token that CubeMaster signed in
// IssueArtifactDownloadTicket, verified locally against the DB-backed store
// and node cache that TC shares with Master (InitForTemplateCenter +
// nodemeta.Init) — not the TC-wide callback/component secret, which must
// never be handed to a node.
func nodeArtifactDownloadHandler(c *gin.Context) {
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	nodeID := strings.TrimSpace(c.GetHeader(artifactticket.DownloadNodeIDHeader))
	token := strings.TrimSpace(c.GetHeader(artifactticket.DownloadTokenHeader))
	if !directArtifactIDPattern.MatchString(artifactID) || nodeID == "" || token == "" {
		c.Status(http.StatusBadRequest)
		return
	}
	artifact, err := verifyNodeArtifactDownload(c.Request.Context(), artifactID, nodeID, token)
	switch {
	case err == nil:
		serveArtifactFile(c, artifact)
	case errors.Is(err, templatecenter.ErrArtifactDownloadUnauthorized):
		c.Status(http.StatusForbidden)
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.Status(http.StatusNotFound)
	case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
		c.Status(http.StatusServiceUnavailable)
	default:
		c.Status(http.StatusInternalServerError)
	}
}

func serveArtifactFile(c *gin.Context, artifact *models.RootfsArtifact) {
	path, err := directArtifactPath(artifact)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	f, err := os.Open(path) // NOCC:Path Traversal (path is constrained below)
	if err != nil {
		if os.IsNotExist(err) {
			c.Status(http.StatusNotFound)
		} else {
			c.Status(http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		c.Status(http.StatusNotFound)
		return
	}

	// Artifact transfers can legitimately outlive the API server's ordinary
	// WriteTimeout. Clear only this response's write deadline.
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
	c.Header("Content-Type", "application/octet-stream")
	c.Header("X-Cube-Artifact-Id", artifact.ArtifactID)
	c.Header("X-Cube-Artifact-SHA256", artifact.Ext4SHA256)
	if checksum := strings.TrimSpace(artifact.Ext4SHA256); checksum != "" {
		c.Header("ETag", strconv.Quote(checksum))
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(path), info.ModTime(), f)
}

func directArtifactPath(artifact *models.RootfsArtifact) (string, error) {
	if artifact == nil {
		return "", errors.New("nil artifact")
	}
	switch strings.ToLower(strings.TrimSpace(artifact.StorageBackend)) {
	case "s3":
		return "", errors.New("s3 artifacts are not served by TemplateCenter")
	case "fs":
		root := filepath.Clean(configenv.ResolveArtifactFSRoot())
		if rawRef := strings.TrimSpace(artifact.StorageRef); rawRef != "" {
			ref, err := url.Parse(rawRef)
			if err != nil || ref.Scheme != "file" || ref.Host != "" || strings.TrimSpace(ref.Path) == "" {
				return "", errors.New("invalid fs artifact storage_ref")
			}
			root = filepath.Clean(ref.Path)
		}
		objectKey := strings.TrimSpace(artifact.ObjectKey)
		if objectKey == "" {
			objectKey = blobstore.ArtifactExt4Key(configenv.EnvOr(configenv.EnvS3ArtifactPrefix), artifact.ArtifactID)
		}
		if err := blobstore.ValidateKey(objectKey); err != nil {
			return "", err
		}
		// blobstore/fs keeps object bytes under <root>/_data/<full-key>.
		dataRoot := filepath.Join(root, "_data")
		candidate := filepath.Clean(filepath.Join(dataRoot, filepath.FromSlash(objectKey)))
		rel, err := filepath.Rel(dataRoot, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("artifact object key escapes fs root")
		}
		return candidate, nil
	default:
		path := filepath.Clean(strings.TrimSpace(artifact.Ext4Path))
		if path == "." || !filepath.IsAbs(path) {
			return "", errors.New("invalid artifact path")
		}
		return path, nil
	}
}

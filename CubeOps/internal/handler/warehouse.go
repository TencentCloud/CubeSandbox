// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	nmhandler "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/warehouse"
)

const nodeIDHeader = "X-Cube-Node-ID"

var (
	nodeIDRe   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	uploadIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// WarehouseHandler serves admin (JWT) and node (internal, no token) warehouse APIs.
type WarehouseHandler struct {
	store      *store.Store
	blobs      warehouse.BlobStore
	importer   *warehouse.Importer
	nodes      nmhandler.NodeService
	presignTTL time.Duration
	uploadMax  int64
}

func NewWarehouseHandler(s *store.Store, blobs warehouse.BlobStore, importer *warehouse.Importer, nodes nmhandler.NodeService, presignTTL time.Duration, uploadMax int64) *WarehouseHandler {
	if presignTTL <= 0 {
		presignTTL = 5 * time.Minute
	}
	if uploadMax <= 0 {
		uploadMax = 8 << 30
	}
	return &WarehouseHandler{
		store:      s,
		blobs:      blobs,
		importer:   importer,
		nodes:      nodes,
		presignTTL: presignTTL,
		uploadMax:  uploadMax,
	}
}

func (h *WarehouseHandler) RegisterAdmin(r *gin.RouterGroup) {
	r.GET("/warehouse/components", h.ListComponents)
	r.GET("/warehouse/components/:component", h.GetComponent)
	r.DELETE("/warehouse/components/:component/versions/:version", h.DeleteVersion)
	r.POST("/warehouse/uploads", h.Upload)
	r.GET("/warehouse/imports", h.ListImports)
	r.POST("/warehouse/imports", h.CreateImport)
	r.GET("/warehouse/imports/:id", h.GetImport)
	r.POST("/warehouse/preinstall", h.CreatePreinstall)
	r.GET("/warehouse/preinstall", h.ListPreinstall)
}

func (h *WarehouseHandler) RegisterInternal(r *gin.RouterGroup) {
	r.GET("/blob", h.GetBlob)
	r.GET("/jobs", h.ListNodeJobs)
	r.POST("/jobs/:id/ack", h.AckJob)
	r.PUT("/inventory", h.PutInventory)
}

func (h *WarehouseHandler) requireEnabled(c *gin.Context) bool {
	if h.blobs == nil {
		httputil.WriteErrorCode(c, http.StatusNotImplemented, warehouse.CodeDisabled,
			"component warehouse is disabled: S3 is not configured")
		return false
	}
	return true
}

func (h *WarehouseHandler) ListComponents(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	items, err := h.store.ListWarehouseItems(c.Request.Context())
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	installs, err := h.store.ListNodeInstalls(c.Request.Context())
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	nodeIDs, coverageOK := h.coverageNodeIDs(c.Request.Context())
	summaries := warehouse.SummarizeComponents(items, installs, nodeIDs, coverageOK)
	httputil.WriteJSON(c, http.StatusOK, gin.H{"components": summaries})
}

func (h *WarehouseHandler) GetComponent(c *gin.Context) {
	component, err := warehouse.NormalizeComponent(c.Param("component"))
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	items, err := h.store.ListWarehouseItemsByComponent(c.Request.Context(), component)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	installs, err := h.store.ListNodeInstalls(c.Request.Context())
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	nodeIDs, coverageOK := h.coverageNodeIDs(c.Request.Context())
	detail := warehouse.GroupComponent(component, items, installs, nodeIDs, coverageOK)
	httputil.WriteJSON(c, http.StatusOK, detail)
}

func (h *WarehouseHandler) coverageNodeIDs(ctx context.Context) (ids []string, ok bool) {
	if h.nodes == nil {
		return nil, false
	}
	snaps, err := h.nodes.ListNodes(ctx)
	if err != nil {
		return nil, false
	}
	for _, n := range snaps {
		if n != nil && n.NodeID != "" {
			ids = append(ids, n.NodeID)
		}
	}
	return ids, true
}

func (h *WarehouseHandler) DeleteVersion(c *gin.Context) {
	component, err := warehouse.NormalizeComponent(c.Param("component"))
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	version, err := warehouse.NormalizeVersion(c.Param("version"))
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	arch, err := warehouse.NormalizeArch(c.Query("arch"))
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "arch query is required (amd64 or arm64)")
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	item, err := h.store.GetWarehouseItem(c.Request.Context(), arch, component, version)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.store.DeleteWarehouseItem(c.Request.Context(), arch, component, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCode(c, http.StatusNotFound, warehouse.CodeNotFound, "warehouse version not found")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	key := storedBlobKey(item, arch, component, version)
	if err := h.blobs.Delete(c.Request.Context(), key); err != nil {
		slog.Warn("warehouse delete object", "key", key, "error", err)
	}
	if err := h.store.CancelPendingPreinstallForVersion(c.Request.Context(), arch, component, version); err != nil {
		slog.Warn("warehouse cancel preinstall after delete", "arch", arch, "component", component, "version", version, "error", err)
	}
	httputil.WriteNoContent(c)
}

func (h *WarehouseHandler) Upload(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	mr, err := c.Request.MultipartReader()
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "multipart body is required")
		return
	}
	var part *multipart.Part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httputil.WriteError(c, http.StatusBadRequest, "invalid multipart body")
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		_ = p.Close()
	}
	if part == nil {
		httputil.WriteError(c, http.StatusBadRequest, "multipart field file is required")
		return
	}
	defer part.Close()
	filename := filepath.Base(part.FileName())
	lower := strings.ToLower(filename)
	if !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") {
		httputil.WriteError(c, http.StatusBadRequest, "upload must be a .tar.gz one-click package")
		return
	}
	id := uuid.NewString()
	key := warehouse.UploadObjectKey(id)
	limited := io.LimitReader(part, h.uploadMax+1)
	info, err := h.blobs.Put(c.Request.Context(), key, limited, "application/gzip")
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "upload failed")
		slog.Error("warehouse upload put failed", "error", err)
		return
	}
	if info.Size > h.uploadMax {
		_ = h.blobs.Delete(c.Request.Context(), key)
		httputil.WriteError(c, http.StatusRequestEntityTooLarge, "upload exceeds size limit")
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, gin.H{"uploadId": id, "filename": filename})
}

type createImportRequest struct {
	Source   string   `json:"source"`
	Repo     string   `json:"repo"`
	Tag      string   `json:"tag"`
	UploadID string   `json:"uploadId"`
	Arch     []string `json:"arch"`
}

func (h *WarehouseHandler) CreateImport(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	var req createImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	arches, err := normalizeArchList(req.Arch)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	source := strings.ToLower(strings.TrimSpace(req.Source))
	sourceRef, err := h.importSourceRef(c.Request.Context(), source, req)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	var jobs []store.ImportJob
	for _, arch := range arches {
		job := store.ImportJob{
			ID:        uuid.NewString(),
			Source:    source,
			SourceRef: sourceRef,
			Arch:      arch,
			Status:    store.ImportPending,
			Tag:       strings.TrimSpace(req.Tag),
		}
		if err := h.store.CreateImportJob(c.Request.Context(), job); err != nil {
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
		jobs = append(jobs, job)
	}
	if h.importer != nil {
		h.importer.Kick()
	}
	httputil.WriteJSON(c, http.StatusAccepted, gin.H{"jobs": jobs})
}

func (h *WarehouseHandler) importSourceRef(ctx context.Context, source string, req createImportRequest) (string, error) {
	switch source {
	case warehouse.SourceUpload:
		id := strings.TrimSpace(req.UploadID)
		if id == "" {
			return "", fmt.Errorf("uploadId is required")
		}
		if !uploadIDRe.MatchString(id) {
			return "", fmt.Errorf("invalid uploadId")
		}
		key := warehouse.UploadObjectKey(id)
		if _, err := h.blobs.Stat(ctx, key); err != nil {
			if warehouse.IsNotExist(err) {
				return "", fmt.Errorf("upload not found")
			}
			return "", fmt.Errorf("stat upload: %w", err)
		}
		return key, nil
	case warehouse.SourceGitHub, warehouse.SourceCNB:
		if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.Tag) == "" {
			return "", fmt.Errorf("repo and tag are required")
		}
		return strings.TrimSpace(req.Repo), nil
	default:
		return "", fmt.Errorf("source must be github, cnb, or upload")
	}
}

func (h *WarehouseHandler) ListImports(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	limit, offset := parsePagination(c)
	jobs, total, err := h.store.ListImportJobs(c.Request.Context(), limit, offset)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []store.ImportJob{}
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"jobs": jobs, "total": total})
}

func (h *WarehouseHandler) GetImport(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	job, err := h.store.GetImportJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if job == nil {
		httputil.WriteError(c, http.StatusNotFound, "import job not found")
		return
	}
	httputil.WriteJSON(c, http.StatusOK, job)
}

type createPreinstallRequest struct {
	NodeIDs   []string `json:"nodeIds"`
	Arch      string   `json:"arch"`
	Component string   `json:"component"`
	Version   string   `json:"version"`
}

func (h *WarehouseHandler) CreatePreinstall(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	var req createPreinstallRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	arch, err := warehouse.NormalizeArch(req.Arch)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	component, err := warehouse.NormalizeComponent(req.Component)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	version, err := warehouse.NormalizeVersion(req.Version)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	item, err := h.store.GetWarehouseItem(c.Request.Context(), arch, component, version)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if item == nil {
		httputil.WriteErrorCode(c, http.StatusNotFound, warehouse.CodeNotFound, "warehouse version not found")
		return
	}
	key := storedBlobKey(item, arch, component, version)
	if _, err := h.blobs.Stat(c.Request.Context(), key); err != nil {
		if warehouse.IsNotExist(err) {
			httputil.WriteErrorCode(c, http.StatusNotFound, warehouse.CodeNotFound, "warehouse version not found")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, "object store unavailable")
		return
	}
	if len(req.NodeIDs) == 0 {
		httputil.WriteError(c, http.StatusBadRequest, "nodeIds is required")
		return
	}
	var jobs []store.PreinstallJob
	for _, rawID := range req.NodeIDs {
		nodeID, err := normalizeNodeID(rawID)
		if err != nil {
			httputil.WriteError(c, http.StatusBadRequest, err.Error())
			return
		}
		jobs = append(jobs, store.PreinstallJob{
			ID:        uuid.NewString(),
			NodeID:    nodeID,
			Arch:      arch,
			Component: component,
			Version:   version,
			Status:    store.PreinstallPending,
		})
	}
	if err := h.store.CreatePreinstallJobs(c.Request.Context(), jobs); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusAccepted, gin.H{"jobs": jobs})
}

func (h *WarehouseHandler) ListPreinstall(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	limit, offset := parsePagination(c)
	jobs, total, err := h.store.ListPreinstallJobs(c.Request.Context(), c.Query("node_id"), c.Query("status"), limit, offset)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []store.PreinstallJob{}
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"jobs": jobs, "total": total})
}

func (h *WarehouseHandler) GetBlob(c *gin.Context) {
	arch, component, version, nodeID, ok := blobTicketArgs(c)
	if !ok {
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	item, err := h.store.GetWarehouseItem(c.Request.Context(), arch, component, version)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "lookup failed")
		return
	}
	if item == nil {
		httputil.WriteErrorCode(c, http.StatusNotFound, warehouse.CodeNotFound, blobMissingMsg(component, version, arch))
		return
	}
	key := storedBlobKey(item, arch, component, version)
	info, err := h.blobs.Stat(c.Request.Context(), key)
	if err != nil {
		if warehouse.IsNotExist(err) {
			httputil.WriteErrorCode(c, http.StatusNotFound, warehouse.CodeNotFound, blobMissingMsg(component, version, arch))
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, "object store unavailable")
		return
	}
	url, err := h.blobs.PresignGet(c.Request.Context(), key, h.presignTTL)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "presign failed")
		return
	}
	checksum := info.SHA256
	if checksum == "" {
		checksum = item.Checksum
	}
	if checksum != "" && !strings.HasPrefix(checksum, "sha256:") {
		checksum = "sha256:" + checksum
	}
	size := info.Size
	if size == 0 {
		size = item.SizeBytes
	}
	slog.Info("warehouse blob issued",
		"node", nodeID, "arch", arch, "component", component, "version", version,
		"key", key, "ttl", int(h.presignTTL.Seconds()))
	httputil.WriteJSON(c, http.StatusOK, gin.H{
		"url":       url,
		"expiresIn": int(h.presignTTL.Seconds()),
		"sizeBytes": size,
		"checksum":  checksum,
	})
}

func (h *WarehouseHandler) ListNodeJobs(c *gin.Context) {
	nodeID, err := requireNodeID(c)
	if err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	jobs, err := h.store.ListNodePreinstallWork(c.Request.Context(), nodeID, 15*time.Minute)
	if err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []store.PreinstallJob{}
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"jobs": jobs})
}

type ackJobRequest struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func (h *WarehouseHandler) AckJob(c *gin.Context) {
	nodeID, err := requireNodeID(c)
	if err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	var req ackJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	status := strings.ToLower(strings.TrimSpace(req.Status))
	switch status {
	case store.PreinstallRunning, store.PreinstallSucceeded, store.PreinstallFailed:
	default:
		httputil.WriteError(c, http.StatusBadRequest, "status must be running, succeeded, or failed")
		return
	}
	if err := h.store.AckPreinstallJob(c.Request.Context(), c.Param("id"), nodeID, status, req.Error); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httputil.WriteErrorCode(c, http.StatusConflict, warehouse.CodeUnauthorizedJob, "job not found for this node")
			return
		}
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"ok": true})
}

type inventoryItemRequest struct {
	Component string `json:"component"`
	Version   string `json:"version"`
}

type inventoryRequest struct {
	Arch  string                 `json:"arch"`
	Items []inventoryItemRequest `json:"items"`
}

func (h *WarehouseHandler) PutInventory(c *gin.Context) {
	nodeID, err := requireNodeID(c)
	if err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if !h.requireEnabled(c) {
		return
	}
	var req inventoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	arch, err := warehouse.NormalizeArch(req.Arch)
	if err != nil {
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
		return
	}
	items := make([]store.NodeInstall, 0, len(req.Items))
	for _, raw := range req.Items {
		component, err := warehouse.NormalizeComponent(raw.Component)
		if err != nil {
			httputil.WriteError(c, http.StatusBadRequest, err.Error())
			return
		}
		version, err := warehouse.NormalizeVersion(raw.Version)
		if err != nil {
			httputil.WriteError(c, http.StatusBadRequest, err.Error())
			return
		}
		items = append(items, store.NodeInstall{Component: component, Version: version})
	}
	if err := h.store.ReplaceNodeInstalls(c.Request.Context(), nodeID, arch, items); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"ok": true})
}

func blobTicketArgs(c *gin.Context) (arch, component, version, nodeID string, ok bool) {
	var err error
	if arch, err = warehouse.NormalizeArch(c.Query("arch")); err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if component, err = warehouse.NormalizeComponent(c.Query("component")); err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if version, err = warehouse.NormalizeVersion(c.Query("version")); err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	if nodeID, err = requireNodeID(c); err != nil {
		httputil.WriteErrorCode(c, http.StatusBadRequest, warehouse.CodeInvalidRequest, err.Error())
		return
	}
	ok = true
	return
}

func storedBlobKey(item *store.WarehouseItem, arch, component, version string) string {
	if item != nil && item.ObjectKey != "" {
		return item.ObjectKey
	}
	return warehouse.ObjectKey(arch, component, version)
}

func blobMissingMsg(component, version, arch string) string {
	return fmt.Sprintf("warehouse does not have %s %s for arch %s", component, version, arch)
}

func requireNodeID(c *gin.Context) (string, error) {
	raw := strings.TrimSpace(c.GetHeader(nodeIDHeader))
	if raw == "" {
		raw = strings.TrimSpace(c.Query("node_id"))
	}
	return normalizeNodeID(raw)
}

func normalizeNodeID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("node_id is required")
	}
	if !nodeIDRe.MatchString(raw) {
		return "", fmt.Errorf("invalid node_id")
	}
	return raw, nil
}

func normalizeArchList(arches []string) ([]string, error) {
	if len(arches) == 0 {
		return nil, fmt.Errorf("arch is required")
	}
	seen := map[string]struct{}{}
	var out []string
	for _, a := range arches {
		n, err := warehouse.NormalizeArch(a)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out, nil
}
